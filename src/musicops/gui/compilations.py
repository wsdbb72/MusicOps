"""Choose source albums, inspect a compilation preview, then apply it."""
from PySide6.QtCore import Qt
from PySide6.QtWidgets import QDialog,QVBoxLayout,QHBoxLayout,QLabel,QLineEdit,QPushButton,QTableWidget,QTableWidgetItem,QHeaderView,QPlainTextEdit


class CompilationDialog(QDialog):
    def __init__(self,owner,parent=None):
        super().__init__(parent);self.owner=owner;self.plan=None;self.rows=[];self.busy=False
        self.setWindowTitle('合并为一张合辑 · 保留逐曲原艺人');self.resize(900,650)
        layout=QVBoxLayout(self);note=QLabel('选择要放在一起的专辑或批次，只统一专辑名称、合辑分组和曲号。每首歌的原艺人、歌名、音频内容保留；原件进入可恢复隔离区。');note.setWordWrap(True);layout.addWidget(note)
        bar=QHBoxLayout();self.query=QLineEdit();self.query.setPlaceholderText('输入原专辑名或艺人，例：最新热歌慢摇');bar.addWidget(self.query);self.search=QPushButton('查找本地专辑');self.search.clicked.connect(self.find);self.query.returnPressed.connect(self.find);bar.addWidget(self.search);layout.addLayout(bar)
        self.table=QTableWidget(0,4);self.table.setHorizontalHeaderLabels(['选择','专辑','专辑艺人','曲目 / 目录']);self.table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch);self.table.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents);self.table.verticalHeader().hide();self.table.itemChanged.connect(self.invalidate);layout.addWidget(self.table,1)
        bar=QHBoxLayout();self.all=QPushButton('全选 / 取消当前结果');self.all.clicked.connect(self.select_all);bar.addWidget(self.all);bar.addStretch();layout.addLayout(bar)
        bar=QHBoxLayout();bar.addWidget(QLabel('合并后的专辑名'));self.album=QLineEdit();self.album.textChanged.connect(self.invalidate);bar.addWidget(self.album);bar.addWidget(QLabel('合辑分组艺人'));self.artist=QLineEdit('群星');self.artist.setToolTip('仅用于专辑分组，不会替换每首歌曲的原艺人');self.artist.textChanged.connect(self.invalidate);bar.addWidget(self.artist);layout.addLayout(bar)
        self.detail=QPlainTextEdit();self.detail.setReadOnly(True);self.detail.setMaximumHeight(140);layout.addWidget(self.detail)
        bar=QHBoxLayout();self.preview=QPushButton('校验并生成合并预览');self.preview.clicked.connect(self.prepare);bar.addWidget(self.preview);self.apply=QPushButton('确认预览并合并');self.apply.setEnabled(False);self.apply.clicked.connect(self.execute);bar.addWidget(self.apply);bar.addStretch();self.close_button=QPushButton('关闭');self.close_button.clicked.connect(self.reject);bar.addWidget(self.close_button);layout.addLayout(bar)
    def reject(self):
        if self.busy:self.detail.setPlainText('正在处理，请等待安全完成后关闭。');return
        super().reject()
    def invalidate(self,*_):
        self.plan=None;self.apply.setEnabled(False)
        if hasattr(self,'preview') and hasattr(self,'table'):
            selected=[r for i,r in enumerate(self.rows) if self.table.item(i,0) and self.table.item(i,0).checkState()==Qt.Checked]
            self.preview.setText(f"校验并生成合并预览（已选 {len(selected)} 项 · {sum(r.get('track_count',0) for r in selected)} 曲）" if selected else '校验并生成合并预览')
    def set_busy(self,busy):
        self.busy=busy
        for widget in (self.query,self.search,self.table,self.all,self.album,self.artist,self.preview,self.close_button):widget.setEnabled(not busy)
        self.apply.setEnabled(not busy and bool(self.plan))
    def fail(self,message):self.detail.setPlainText(message);self.set_busy(False)
    def find(self):
        self.invalidate();self.set_busy(True)
        def done(result):
            self.rows=result['albums'];self.table.blockSignals(True);self.table.setUpdatesEnabled(False);self.table.setRowCount(len(self.rows))
            for i,row in enumerate(self.rows):
                item=QTableWidgetItem();item.setFlags(Qt.ItemIsEnabled|Qt.ItemIsUserCheckable);item.setCheckState(Qt.Unchecked);self.table.setItem(i,0,item)
                for col,text in enumerate((row['album'],' / '.join(row['artists']),str(row['track_count'])+' 曲 · '+row['directory']),1):
                    item=QTableWidgetItem(text);item.setFlags(Qt.ItemIsEnabled|Qt.ItemIsSelectable);item.setToolTip(text);self.table.setItem(i,col,item)
            self.table.setUpdatesEnabled(True);self.table.blockSignals(False)
            if self.rows and not self.album.text().strip() and self.query.text().strip():self.album.setText(self.query.text().strip())
            self.invalidate()
            self.detail.setPlainText('仅显示前200项，请缩小关键词。' if result.get('truncated') else f'找到{len(self.rows)}项，请勾选后填写合并后的专辑名。');self.set_busy(False)
        self.owner.call('compilation_albums',{'query':self.query.text()},done,self.fail)
    def select_all(self):
        items=[self.table.item(i,0) for i in range(len(self.rows))];state=Qt.Unchecked if items and all(i.checkState()==Qt.Checked for i in items) else Qt.Checked
        self.table.blockSignals(True)
        for item in items:item.setCheckState(state)
        self.table.blockSignals(False);self.invalidate()
    def prepare(self):
        selected=[r for i,r in enumerate(self.rows) if self.table.item(i,0).checkState()==Qt.Checked];ids=list(dict.fromkeys(fid for r in selected for fid in r['file_ids']))
        if not ids:self.fail('请先选择要合并的专辑。');return
        self.invalidate();self.set_busy(True);self.detail.setPlainText('正在校验所选音频并准备独立副本，原音乐库尚未改动…')
        def done(result):
            self.plan=result['plan_id'];self.detail.setPlainText(f"将{len(selected)}个专辑分组合并为《{result['album']}》，共{result['tracks']}首。\n合辑分组艺人：{result['albumartist']}。每首歌曲的原艺人保持不变。\n目标：{result['keep_directory']}\n{result['reason']}");self.set_busy(False)
        self.owner.call('preview_compilation',{'file_ids':ids,'album':self.album.text(),'albumartist':self.artist.text()},done,self.fail)
    def execute(self):
        if not self.plan:return
        self.set_busy(True);self.detail.setPlainText('正在按预览合并，完成后可在去重页恢复…')
        def done(result):self.plan=None;self.detail.setPlainText(result['message']);self.set_busy(False)
        self.owner.call('merge_compilation',{'plan_id':self.plan},done,self.fail)
