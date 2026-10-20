"""Scan, select reliable groups, merge and restore without exposing raw plans."""
from PySide6.QtCore import Qt
from PySide6.QtWidgets import QWidget,QVBoxLayout,QHBoxLayout,QLabel,QPushButton,QTableWidget,QTableWidgetItem,QHeaderView,QAbstractItemView,QPlainTextEdit

class DuplicatePage(QWidget):
    def __init__(self,owner):
        super().__init__(owner);self.owner=owner;self.report=None;self.last_transactions=[];self.busy=False;self.merging=False;self.pending_task=None
        layout=QVBoxLayout(self);layout.setContentsMargins(0,0,0,0)
        self.note=QLabel('仅扫描候选文件，按需完整校验。不同发行、母带、声道和资料冲突不会自动合并。');self.note.setWordWrap(True);layout.addWidget(self.note)
        row=QHBoxLayout();self.scan_button=QPushButton('扫描歌曲与专辑重复');self.scan_button.clicked.connect(self.scan);row.addWidget(self.scan_button)
        self.compilation_button=QPushButton('合并为一张合辑');self.compilation_button.clicked.connect(self.compilation);row.addWidget(self.compilation_button)
        self.select_button=QPushButton('勾选 / 取消可靠项');self.select_button.clicked.connect(self.select_ready);row.addWidget(self.select_button);self.stop_button=QPushButton('停止当前扫描 / 合并');self.stop_button.clicked.connect(self.stop_current);row.addWidget(self.stop_button);row.addStretch();layout.addLayout(row)
        self.table=QTableWidget(0,5);self.table.setHorizontalHeaderLabels(['选择','专辑 / 艺人','副本 / 曲目','处理方式','结果']);self.table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch);self.table.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents);self.table.horizontalHeader().setSectionResizeMode(1,QHeaderView.Interactive);self.table.setColumnWidth(1,230);self.table.horizontalHeader().setSectionResizeMode(2,QHeaderView.ResizeToContents);self.table.horizontalHeader().setSectionResizeMode(3,QHeaderView.ResizeToContents);self.table.setAlternatingRowColors(True);self.table.verticalHeader().setDefaultSectionSize(44);self.table.verticalHeader().hide();self.table.setMinimumHeight(220);self.table.setSelectionBehavior(QAbstractItemView.SelectRows);self.table.setEditTriggers(QAbstractItemView.NoEditTriggers);self.table.itemSelectionChanged.connect(self.details);self.table.itemChanged.connect(lambda _:self.update_selection_state());layout.addWidget(self.table,1)
        self.detail=QPlainTextEdit();self.detail.setReadOnly(True);self.detail.setFixedHeight(100);self.detail.setPlaceholderText('选择项目查看保留目录、文件和不能自动合并的原因。');layout.addWidget(self.detail)
        row=QHBoxLayout();self.merge_button=QPushButton('自动合并勾选项');self.merge_button.clicked.connect(lambda:self.merge(False));row.addWidget(self.merge_button);self.merge_all_button=QPushButton('自动合并全部可靠项');self.merge_all_button.clicked.connect(lambda:self.merge(True));row.addWidget(self.merge_all_button);row.addStretch();self.restore_button=QPushButton('恢复上次合并');self.restore_button.clicked.connect(self.restore);self.restore_button.setEnabled(False);row.addWidget(self.restore_button);layout.addLayout(row);self.set_busy(False)
    def load_history(self):
        if self.busy:return
        def received(result):
            self.last_transactions=[t['tx_id'] for t in result['transactions']];self.restore_button.setEnabled(not self.busy and bool(self.last_transactions))
        self.owner.call('duplicate_history',{},received,self.failed)
    def compilation(self):
        from musicops.gui.compilations import CompilationDialog
        dialog=CompilationDialog(self.owner,self)
        dialog.exec();self.load_history()
    def set_busy(self,busy):
        self.busy=busy
        if not busy:self.pending_task=None
        self.stop_button.setEnabled(busy and bool(self.pending_task))
        self.compilation_button.setEnabled(not busy)
        self.scan_button.setEnabled(not busy);self.select_button.setEnabled(not busy and bool(self.report and self.report.get('ready_count')));self.merge_button.setEnabled(not busy and bool(self.report and self.report.get('ready_count')));self.merge_all_button.setEnabled(not busy and bool(self.report and self.report.get('ready_count')));self.restore_button.setEnabled(not busy and bool(self.last_transactions))
        self.update_selection_state()
    def update_selection_state(self):
        ready=int(self.report.get('ready_count',0)) if self.report else 0
        checked=sum(1 for i,g in enumerate(self.report['groups']) if g['state']=='ready' and self.table.item(i,0) and self.table.item(i,0).checkState()==Qt.Checked) if self.report and self.table.rowCount()==len(self.report.get('groups',[])) else 0
        self.merge_button.setText(f'自动合并勾选项（已选 {checked} 项）' if checked else '自动合并勾选项')
        self.merge_all_button.setText(f'自动合并全部可靠项（{ready} 项）' if ready else '自动合并全部可靠项')
    def failed(self,message):
        self.note.setText(message);self.set_busy(False)
        if self.merging:
            self.merging=False;self.load_history()
    def scan(self):
        self.report=None;self.table.setRowCount(0);self.detail.clear()
        self.set_busy(True);self.note.setText('等待后台扫描并校验重复候选；已有任务会先完成。')
        self.owner.call('scan_duplicates',{},self.received,self.failed,accepted=self.accepted)
    def accepted(self,result):
        self.pending_task=result['task_id'];self.stop_button.setEnabled(True)
    def stop_current(self):
        if not self.pending_task:return
        self.stop_button.setEnabled(False)
        def failed(message):self.note.setText(message);self.stop_button.setEnabled(bool(self.busy and self.pending_task))
        self.owner.call('cancel_task',{'task_id':self.pending_task},lambda _:self.note.setText('已请求停止；当前文件或专辑事务会到安全点，不继续下一项。已完成部分仍可恢复。'),failed)
    def received(self,result):
        self.report=result;self.table.setUpdatesEnabled(False);self.table.blockSignals(True)
        try:
            self.table.setRowCount(len(result['groups']))
            for index,group in enumerate(result['groups']):
                item=QTableWidgetItem();item.setData(Qt.UserRole,group['group_id'])
                if group['state']=='ready':item.setFlags(item.flags()|Qt.ItemIsUserCheckable);item.setCheckState(Qt.Unchecked)
                else:item.setFlags(item.flags()&~Qt.ItemIsUserCheckable)
                self.table.setItem(index,0,item)
                for col,value in enumerate([group['album']+' / '+' / '.join(group['artists']),str(group['directory_count'])+' 个目录 · '+str(group['file_count'])+' 曲','可自动合并' if group['state']=='ready' else '需要核对',group['reason']],1):
                    item=QTableWidgetItem(value);item.setToolTip(value);self.table.setItem(index,col,item)
        finally:
            self.table.blockSignals(False);self.table.setUpdatesEnabled(True)
        if result['groups']:self.table.selectRow(0);self.details()
        self.note.setText(f"找到 {result['ready_count']} 项可自动合并，{result['review_count']} 项需要核对。"+('本次数量较多，合并后可重新扫描其余项目。' if result.get('truncated') else '')+'旧副本进入隔离区，支持恢复；做种源不改动。');self.set_busy(False)
    def details(self):
        if not self.report or self.table.currentRow()<0:return
        group=self.report['groups'][self.table.currentRow()]
        removed=f"（将移出重复副本 {group['removed_tracks']} 首）" if group.get('removed_tracks') else ''
        self.detail.setPlainText(group['reason']+removed+'\n保留目录：'+group.get('keep_directory','等待核对')+'\n'+'\n'.join(f['path'] for f in group['files'])+('\n仅列出前50个文件；本组共 '+str(group['file_count'])+' 曲。' if group['file_count']>len(group['files']) else ''))
    def select_ready(self):
        items=[self.table.item(i,0) for i,g in enumerate(self.report['groups']) if g['state']=='ready'] if self.report else []
        target=Qt.Unchecked if items and all(i.checkState()==Qt.Checked for i in items) else Qt.Checked
        self.table.setUpdatesEnabled(False);self.table.blockSignals(True)
        try:
            for item in items:item.setCheckState(target)
        finally:
            self.table.blockSignals(False);self.table.setUpdatesEnabled(True)
        self.update_selection_state()
    def merge(self,all_ready):
        if not self.report:return
        ids=[g['group_id'] for i,g in enumerate(self.report['groups']) if g['state']=='ready' and (all_ready or self.table.item(i,0).checkState()==Qt.Checked)]
        if not ids:self.note.setText('请先勾选可自动合并的项目。');return
        self.merging=True;self.set_busy(True);self.note.setText('正在核验文件并合并，旧副本保留恢复记录…')
        self.owner.call('merge_duplicates',{'scan_id':self.report['scan_id'],'group_ids':ids},self.merged,self.failed,accepted=self.accepted)
    def merged(self,result):
        self.merging=False
        self.last_transactions=[t['tx_id'] for t in result['transactions'] if t['state']=='COMMITTED'];self.note.setText(result['message']);self.report=None;self.table.setRowCount(0);self.detail.clear();self.set_busy(False)
    def restore(self):
        if not self.last_transactions:return
        self.set_busy(True);self.note.setText('正在按合并记录逐项恢复…');tx=self.last_transactions[-1]
        def done(result):
            if result['state']=='ROLLED_BACK':self.last_transactions.pop()
            if self.last_transactions:self.restore()
            else:self.note.setText('上次合并已恢复。');self.set_busy(False)
        self.owner.call('restore_duplicates',{'tx_id':tx},done,self.failed)
