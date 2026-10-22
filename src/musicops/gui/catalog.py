from PySide6.QtCore import Qt
from PySide6.QtWidgets import QDialog, QVBoxLayout, QHBoxLayout, QLabel, QPushButton, QCheckBox, QTableWidget, QTableWidgetItem, QHeaderView, QAbstractItemView


class LibraryCatalogDialog(QDialog):
    def __init__(self, owner, single_upgrade=False):
        super().__init__(owner); self.owner = owner; self.offset = 0; self.total = 0; self.closed = False
        self.finished.connect(lambda _: setattr(self, 'closed', True)); self.setWindowTitle('本地专辑资料库'); self.resize(900, 580)
        layout = QVBoxLayout(self); self.note = QLabel('正在读取本地资料库…'); self.note.setWordWrap(True); layout.addWidget(self.note)
        self.table = QTableWidget(0, 5); self.table.setHorizontalHeaderLabels(['专辑', '艺人', '曲目数', '预匹配', '目录']); self.table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch); self.table.verticalHeader().setVisible(False); self.table.verticalHeader().setDefaultSectionSize(44); self.table.setAlternatingRowColors(True); self.table.setEditTriggers(QAbstractItemView.NoEditTriggers); layout.addWidget(self.table)
        self.table.setSelectionBehavior(QAbstractItemView.SelectRows);self.table.setSelectionMode(QAbstractItemView.SingleSelection)
        if single_upgrade:
            self.setWindowTitle('选择要升级为完整专辑的单曲发行')
            hint=QLabel('先选原单曲或部分曲目所在专辑，再核对它收录到的完整专辑。曲目少不等于 Single；不会自动改名冒充完整专辑。');hint.setWordWrap(True);layout.addWidget(hint)
            self.retire=QCheckBox('完整专辑入库且对应歌曲音质不下降后，将原单曲移入可恢复备份（默认保留）');layout.addWidget(self.retire)
            self.find_full=QPushButton('查找所选单曲对应的完整专辑');self.find_full.clicked.connect(self.expand_single);layout.addWidget(self.find_full)
        row = QHBoxLayout(); back = QPushButton('上一页'); back.clicked.connect(lambda: self.page(-200)); row.addWidget(back); forward = QPushButton('下一页'); forward.clicked.connect(lambda: self.page(200)); row.addWidget(forward);refresh=QPushButton('刷新列表');refresh.clicked.connect(self.load);row.addWidget(refresh); row.addStretch(); close = QPushButton('关闭'); close.clicked.connect(self.accept); row.addWidget(close); layout.addLayout(row); self.load()
    def expand_single(self):
        index=self.table.currentRow()
        if index<0:self.note.setText('请先选择原单曲所在专辑；列表为空时先更新本地专辑资料库。');return
        album_id=self.table.item(index,0).data(Qt.UserRole)
        self.owner.start('expand',album_id=album_id,retire_single=self.retire.isChecked());self.accept()
    def page(self, delta):
        self.offset = max(0, min(max(0, (self.total - 1) // 200 * 200), self.offset + delta)); self.load()
    def load(self):
        self.owner.call('list_library_albums', {'offset': self.offset}, self.received, lambda error: self.note.setText(error) if not self.closed else None)
    def received(self, result):
        if self.closed: return
        self.total = result['total']; self.note.setText(f'共 {self.total} 张专辑 · 当前第 {self.offset // 200 + 1} 页。按本地标签预匹配；正式发行、版本冲突仍会进入人工核对。')
        rows = result['albums']; self.table.setRowCount(len(rows))
        for index, album in enumerate(rows):
            for column, value in enumerate([album['album'], ' / '.join(album['artists']), str(album['track_count']), '本地资料已对应' if album['state'] == 'local_tags_matched' else '需要补全资料', album['directory']]):
                item = QTableWidgetItem(value); item.setToolTip(value); self.table.setItem(index, column, item)
            self.table.item(index,0).setData(Qt.UserRole,album['album_id'])
