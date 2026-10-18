"""Native platform credentials and QR login; no external configuration page."""
import base64
import io
from PySide6.QtCore import QTimer, Qt
from PySide6.QtGui import QPixmap
from PySide6.QtWidgets import QDialog, QVBoxLayout, QHBoxLayout, QLabel, QPushButton, QLineEdit
from musicops.gui.controls import QComboBox

PLATFORMS = (('netease', '网易云音乐'), ('qq', 'QQ 音乐'), ('qq_wx', 'QQ 音乐（微信扫码）'),
    ('kuwo', '酷我音乐'), ('kugou', '酷狗音乐'), ('migu', '咪咕音乐'), ('soda', '汽水音乐'),
    ('bilibili', 'bilibili'), ('apple', 'Apple Music'))


class EngineLogin(QDialog):
    def __init__(self, owner, saved_refs=()):
        super().__init__(owner)
        self.owner = owner
        self.saved_refs = set(saved_refs)
        self.session = None
        self.busy = False
        self.generation = 0
        self.setWindowTitle('流媒体平台登录')
        self.resize(560, 610)
        layout = QVBoxLayout(self)
        self.platform = QComboBox()
        for source, title in PLATFORMS: self.platform.addItem(title, source)
        layout.addWidget(self.platform)
        hint = QLabel('公开音源可跳过登录。需要账号时，粘贴该平台 Cookie（Cookie-Editor JSON 或 name=value; …），或扫码登录。\nApple Music 需要有效订阅及 media-user-token；仅试听的音源不会入库。')
        hint.setWordWrap(True); layout.addWidget(hint)
        self.cookie = QLineEdit(); self.cookie.setEchoMode(QLineEdit.Password); self.cookie.setMaxLength(1_000_000); layout.addWidget(self.cookie)
        buttons = QHBoxLayout()
        self.save = QPushButton('加密保存 Cookie'); self.save.clicked.connect(self.save_cookie); buttons.addWidget(self.save)
        self.qr = QPushButton('生成 / 刷新二维码'); self.qr.clicked.connect(self.create); buttons.addWidget(self.qr)
        layout.addLayout(buttons)
        self.image = QLabel(); self.image.setAlignment(Qt.AlignCenter); self.image.setMinimumHeight(280); layout.addWidget(self.image)
        self.note = QLabel(); self.note.setWordWrap(True); layout.addWidget(self.note)
        close = QPushButton('完成'); close.clicked.connect(self.accept); layout.addWidget(close)
        self.timer = QTimer(self); self.timer.setInterval(2500); self.timer.timeout.connect(self.poll)
        self.platform.currentIndexChanged.connect(self.changed)
        self.finished.connect(lambda _: self.timer.stop())
        self.changed()

    def changed(self):
        self.generation += 1; self.session = None; self.timer.stop(); self.image.clear(); self.cookie.clear()
        source = self.platform.currentData(); source = 'qq' if source == 'qq_wx' else source
        self.cookie.setPlaceholderText('••••••••  ·  已保存，留空保留' if 'vault://engine/' + source in self.saved_refs else '尚未填写，可跳过')
        self.save.setEnabled(self.platform.currentData() != 'qq_wx')
        self.note.setText('没有二维码登录能力的平台请使用 Cookie；可随时关闭并跳过。')

    def request(self, payload, callback):
        if self.busy: return
        self.busy = True; generation = self.generation
        def done(result):
            self.busy = False
            if generation == self.generation and self.isVisible(): callback(result)
        def failed(message):
            self.busy = False
            if generation == self.generation: self.timer.stop(); self.note.setText('未完成：' + message)
        self.owner.call('engine_control', payload, done, failed)

    def save_cookie(self):
        if not self.cookie.text().strip(): return
        source = self.platform.currentData()
        def saved(result):
            self.saved_refs.add('vault://engine/' + source); self.changed(); self.note.setText(result['message'])
        self.request({'action': 'cookie', 'source': source, 'cookie': self.cookie.text()}, saved)

    def create(self):
        self.timer.stop(); self.note.setText('正在启动内置引擎并生成二维码…')
        def created(result):
            self.session = result['session_id']; pixmap = QPixmap()
            try:
                if result.get('image'):data=base64.b64decode(result['image'].split(',',1)[1],validate=True)
                else:
                    import qrcode
                    output = io.BytesIO(); qrcode.make(result['url']).save(output, format='PNG'); data = output.getvalue()
                if len(data) > 2_000_000 or not pixmap.loadFromData(data): raise ValueError()
            except (ValueError, KeyError):
                self.note.setText('二维码格式无效，请改用 Cookie 登录'); return
            self.image.setPixmap(pixmap.scaled(280, 280, Qt.KeepAspectRatio, Qt.SmoothTransformation))
            self.note.setText('请用对应平台 App 扫码，并在手机上确认登录。'); self.timer.start()
        self.request({'action': 'create', 'source': self.platform.currentData()}, created)

    def poll(self):
        if not self.session or self.busy: return
        def checked(result):
            self.note.setText(result['message'])
            if result.get('saved'):
                self.timer.stop(); self.image.clear()
                source = self.platform.currentData(); self.saved_refs.add('vault://engine/' + ('qq' if source == 'qq_wx' else source)); self.cookie.setPlaceholderText('••••••••  ·  已保存，留空保留')
            elif result.get('status') in ('expired', 'failed', 'error'): self.timer.stop()
        self.request({'action': 'poll', 'source': self.platform.currentData(), 'session_id': self.session}, checked)
