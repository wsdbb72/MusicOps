"""Three user workflows and a skippable connection wizard."""
from __future__ import annotations
import copy,json,os
from pathlib import Path
from PySide6.QtCore import QObject,QRunnable,QThreadPool,QTimer,Qt,Signal,QPropertyAnimation,QEasingCurve,QSize
from PySide6.QtGui import QFont,QDesktopServices,QIcon,QPixmap,QPainter,QPen,QColor,QPalette
from PySide6.QtCore import QUrl
from PySide6.QtWidgets import (QApplication,QMainWindow,QWidget,QVBoxLayout,QHBoxLayout,QLabel,QPushButton,QLineEdit,QPlainTextEdit,QFileDialog,QCheckBox,QGroupBox,QFormLayout,QWizard,QWizardPage,QListWidget,QStackedWidget,QTableWidget,QTableWidgetItem,QHeaderView,QMessageBox,QSpinBox,QScrollArea,QInputDialog,QDialog,QDialogButtonBox,QGraphicsOpacityEffect,QProgressBar,QAbstractItemView,QComboBox,QRadioButton)
from musicops.worker.client import WorkerClient
from musicops.clients.http import redact
from musicops.gui.branding import app_icon,apply_native_titlebar
from musicops.providers.playlist_input import PLATFORMS
from musicops.providers.playlist_access import HELP as PLAYLIST_AUTH_HELP

SITES=[('open.cd','OpenCD','www.open.cd'),('dicmusic','DicMusic','dicmusic.com'),('orpheus','Orpheus','orpheus.network'),('redacted','RED','redacted.sh')]
SCENES={'playlist':'歌单入库','complete':'资料补全','upgrade':'音质升级'}
STAGES={'preparing':'准备专辑任务','snapshotting':'核对原音源','queued':'已排队','restored':'已恢复原版','enrich':'补全资料','planning':'分析曲目','searching':'寻找音源','requesting':'获取下载资源','downloading':'下载中','fallback':'网易云下载','metadata':'整理专辑资料','validating':'检查完整音频','publishing':'写入音乐库','retiring':'移出旧版','notifying':'通知音乐服务器','running':'正在处理','completed':'已完成','needs_review':'需要核对','failed':'未完成','cancelled':'已取消','skipped':'已跳过','waiting':'等待处理'}

def eye_icon():
    image=QPixmap(20,20);image.fill(Qt.transparent);painter=QPainter(image);painter.setRenderHint(QPainter.Antialiasing);painter.setPen(QPen(QColor('#91a7c2'),1.6));painter.drawEllipse(2,5,16,10);painter.drawEllipse(8,8,4,4);painter.end();return QIcon(image)

def nav_icon(index):
    image=QPixmap(22,22);image.fill(Qt.transparent);p=QPainter(image);p.setRenderHint(QPainter.Antialiasing);p.setPen(QPen(QColor('#8ed6d4'),1.6,Qt.SolidLine,Qt.RoundCap,Qt.RoundJoin))
    if index==0:
        for y in (5,10,15):p.drawLine(3,y,13,y)
        p.drawLine(18,8,18,17);p.drawLine(18,8,21,7);p.drawEllipse(14,16,4,3)
    elif index==1:
        p.drawRoundedRect(3,3,16,16,3,3);p.drawEllipse(6,6,4,4);p.drawLine(7,14,15,14);p.drawLine(7,17,13,17)
    elif index==2:
        p.drawLine(4,18,4,13);p.drawLine(9,18,9,10);p.drawLine(14,18,14,7);p.drawLine(19,18,19,3);p.drawLine(12,4,19,3);p.drawLine(19,3,20,9)
    elif index==3:
        p.drawRoundedRect(3,3,16,16,3,3)
        for y in (7,11,15):p.drawLine(7,y,8,y);p.drawLine(11,y,16,y)
    else:p.drawEllipse(3,3,12,12);p.drawLine(13,13,20,20)
    p.end();return QIcon(image)

def dark_palette():
    palette=QPalette()
    for role,color in ((QPalette.Window,'#0d1523'),(QPalette.WindowText,'#e6eef8'),(QPalette.Base,'#101b2d'),(QPalette.AlternateBase,'#16253a'),(QPalette.Text,'#e6eef8'),(QPalette.Button,'#1b2b42'),(QPalette.ButtonText,'#d4e3f4'),(QPalette.Light,'#344961'),(QPalette.Mid,'#24334a'),(QPalette.Dark,'#0d1523'),(QPalette.Highlight,'#256a6b'),(QPalette.HighlightedText,'#ffffff'),(QPalette.PlaceholderText,'#8b9eb7')):palette.setColor(role,QColor(color))
    return palette

class CredentialField(QLineEdit):
    """Saved state is a placeholder, never an editable or transmitted secret."""
    def __init__(self,placeholder='',saved=False):
        super().__init__();self.saved=bool(saved);self.setMaxLength(1_000_000);self.setEchoMode(QLineEdit.Password);self.setMinimumHeight(42)
        self.setPlaceholderText('••••••••  ·  已保存，输入即可替换' if self.saved else placeholder)
        self.setToolTip('已保存在本机；不输入新内容会保留原值。' if self.saved else placeholder)
        self.setAccessibleDescription('已保存；留空保留' if self.saved else '尚未填写')
        self.visibility_action=self.addAction(eye_icon(),QLineEdit.TrailingPosition);self.visibility_action.setText('显示 / 隐藏新填写的内容');self.visibility_action.setToolTip('显示 / 隐藏新填写的内容');self.visibility_action.triggered.connect(self.toggle_visibility)
    def toggle_visibility(self):self.setEchoMode(QLineEdit.Normal if self.echoMode()==QLineEdit.Password else QLineEdit.Password)
    def toPlainText(self):return self.text()
    def setPlainText(self,value):self.setText(value)

class Signals(QObject):
    done=Signal(object);failed=Signal(str)
class Request(QRunnable):
    def __init__(self,client,command,payload):super().__init__();self.client=client;self.command=command;self.payload=payload;self.signals=Signals()
    def run(self):
        try:self.signals.done.emit(self.client.ensure_running() if self.command=='attach' else self.client.call(self.command,self.payload))
        except Exception as e:self.signals.failed.emit(str(redact(str(e))))
        finally:self.payload={}

def line(value='',placeholder='',password=False):
    w=QLineEdit(str(value));w.setPlaceholderText(placeholder)
    if password:w.setEchoMode(QLineEdit.Password)
    return w
def label(text,kind=None):
    w=QLabel(text);w.setWordWrap(True);w.setTextFormat(Qt.PlainText)
    if kind:w.setObjectName(kind)
    return w
def button(text,callback,primary=False):
    w=QPushButton(text);w.clicked.connect(callback)
    if primary:w.setObjectName('primary')
    return w
def browse_field(value,folder=True):
    root=QWidget();box=QHBoxLayout(root);box.setContentsMargins(0,0,0,0);edit=line(value)
    def choose():
        path=QFileDialog.getExistingDirectory(root,'选择目录',edit.text()) if folder else QFileDialog.getOpenFileName(root,'选择程序',edit.text(),'程序 (*.exe);;所有文件 (*)' if os.name=='nt' else '可执行文件 (*)')[0]
        if path:edit.setText(path)
    box.addWidget(edit);box.addWidget(button('选择…',choose));return root,edit

class SetupWizard(QWizard):
    def __init__(self,owner):
        super().__init__(owner);self.owner=owner;self.values=copy.deepcopy(owner.config);self.setWindowTitle('MusicOps · 连接与设置');self.setWindowIcon(app_icon());self.setPalette(dark_palette());self.setWizardStyle(QWizard.ClassicStyle);self.resize(900,760);self.setMinimumSize(760,640)
        self.setButtonText(QWizard.NextButton,'继续');self.setButtonText(QWizard.BackButton,'上一步');self.setButtonText(QWizard.FinishButton,'保存并开始使用');self.setButtonText(QWizard.CancelButton,'稍后设置')
        self.setOption(QWizard.HaveCustomButton1,True);self.setButtonText(QWizard.CustomButton1,'跳过本步');self.customButtonClicked.connect(lambda _:self.accept() if self.currentId()==self.pageIds()[-1] else self.next())
        self.fields={};self.site_fields={};self.saved_refs=set(getattr(owner,'available_refs',[]))
        p=self._add_page('音乐放在哪里','选择你希望 Roon / Navidrome 读取的音乐库。下载与整理的临时数据由 MusicOps 自动管理。')
        row,self.library=browse_field(self.values.get('paths',{}).get('library',''));p.addRow('音乐库目录',row)
        self.roon=QCheckBox('让 Roon 读取这个目录');self.roon.setChecked(self.values.get('music_servers',{}).get('roon',{}).get('enabled',False));p.addRow(self.roon)
        p.addRow(label('如果使用 Roon，请在 Roon → 设置 → 存储中添加同一目录。可以稍后再连接。','hint'))
        p=self._add_page('连接四个资源站','有账号的站点才需要填写。没有账号就保持关闭，之后可以随时添加。')
        for key,name,host in SITES:
            group=QGroupBox(name);layout=QVBoxLayout(group);enabled=QCheckBox('参与检索');settings=self.values.get('providers',{}).get(key,{})
            enabled.setChecked(settings.get('enabled',False));layout.addWidget(enabled)
            stored=settings.get('credential_ref') in self.saved_refs
            help_text='● 已保存 Cookie · 留空保留，导入或粘贴新 JSON 即可替换。' if stored else '登录网站 → Cookie-Editor → 导出 JSON → 导入文件或粘贴。'
            layout.addWidget(label(help_text,'credentialSaved' if stored else 'hint'));cookie=CredentialField('粘贴 Cookie-Editor 导出的 JSON',stored);layout.addWidget(cookie)
            actions=QHBoxLayout()
            def load_json(textbox=cookie,check=enabled):
                path=QFileDialog.getOpenFileName(self,'导入 JSON Cookie','','Cookie JSON (*.json *.txt);;所有文件 (*)')[0]
                if path:
                    try:
                        file=Path(path)
                        if file.stat().st_size>1_000_000:raise ValueError('文件过大，请选择 Cookie 导出文件')
                        data=file.read_text('utf-8-sig');json.loads(data);textbox.setPlainText(data);check.setChecked(True)
                    except Exception:QMessageBox.information(self,'无法导入','请使用 Cookie-Editor 导出的 JSON 文件。')
            actions.addWidget(button('导入 JSON 文件',load_json));actions.addWidget(button('打开站点',lambda checked=False,h=host:QDesktopServices.openUrl(QUrl('https://'+h))));actions.addStretch();layout.addLayout(actions)
            p.addRow(group);self.site_fields[key]=(enabled,cookie)
        p=self._add_page('连接 qBittorrent','用于接收站点种子并下载。qBittorrent → 选项 → WebUI，启用 Web 用户界面；把地址与登录账号填在这里。')
        cfg=self.values.get('qbt',{});qbt_saved=cfg.get('credential_ref') in self.saved_refs;self.qbt_url=line(cfg.get('base_url','http://127.0.0.1:8080'),'例如 http://127.0.0.1:8080');self.qbt_user=CredentialField('WebUI 用户名',qbt_saved);self.qbt_pass=CredentialField('WebUI 密码',qbt_saved)
        p.addRow('WebUI 地址',self.qbt_url);p.addRow('用户名',self.qbt_user);p.addRow('密码',self.qbt_pass)
        p.addRow(label('● 已保存登录信息 · 留空保留；更新时请同时填写用户名与密码。' if qbt_saved else '没有 qBittorrent 时可跳过；资料补全仍可使用。','credentialSaved' if qbt_saved else 'hint'))
        fl_row=QWidget();fl_layout=QHBoxLayout(fl_row);fl_layout.setContentsMargins(0,0,0,0);self.fl_limit=QSpinBox();self.fl_limit.setRange(0,100);self.fl_limit.setValue(self.values.get('workflow',{}).get('fl_per_batch',3));self.fl_limit.setSuffix(' 次 / 批次');self.fl_unlimited=QCheckBox('全部');self.fl_unlimited.setToolTip('不限制本批次使用 FL 的次数；仍仅请求站点确认免费的下载。');self.fl_unlimited.setChecked(self.values.get('workflow',{}).get('fl_unlimited',False));self.fl_limit.setDisabled(self.fl_unlimited.isChecked());self.fl_unlimited.toggled.connect(self.fl_limit.setDisabled);fl_layout.addWidget(self.fl_limit,1);fl_layout.addWidget(self.fl_unlimited);p.addRow('最多使用 FL 免费下载',fl_row)
        p.addRow(label('勾选“全部”后忽略左侧次数，按本批次需要使用 FL。OpenCD 直接获取种子；其他站点仅使用已确认免费的下载，额度不足时等待处理。','hint'))
        p=self._add_page('网易云回退与媒体工具','四站没有合适音源时使用网易云。无法获取的曲目会保留说明，不会用其他歌曲冒充。')
        flow=self.values.get('workflow',{});self.fallback=QCheckBox('启用网易云回退');self.fallback.setChecked(flow.get('netease_fallback',True));p.addRow(self.fallback)
        self.netease_api=line(flow.get('netease_api_url',''),'可选：本地网易云 API 服务地址');p.addRow('API 服务（可选）',self.netease_api)
        row,self.import_dir=browse_field(flow.get('netease_import_dir',''));p.addRow('客户端下载目录（可选）',row)
        netease_saved=(self.values.get('netease',{}).get('credential_ref') or self.values.get('providers',{}).get('netease',{}).get('credential_ref')) in self.saved_refs;self.netease_cookie=CredentialField('可选：网易云 Cookie-Editor JSON',netease_saved);p.addRow('网易云登录态（可选）',self.netease_cookie)
        def import_netease():
            path=QFileDialog.getOpenFileName(self,'导入网易云 JSON Cookie','','Cookie JSON (*.json *.txt)')[0]
            if path:
                try:
                    file=Path(path)
                    if file.stat().st_size>500_000:raise ValueError()
                    content=file.read_text('utf-8-sig');json.loads(content);self.netease_cookie.setPlainText(content)
                except Exception:QMessageBox.information(self,'无法导入','请使用网易云 Cookie-Editor 导出的 JSON 文件。')
        p.addRow(button('导入网易云 JSON Cookie',import_netease))
        p.addRow(label('未设置 API 时使用公开可下载音源。已有客户端下载的 NCM 可从上述目录接入，自动解密工作副本。需要账户授权的歌曲仍需你的下载器或 API 提供权限。','hint'))
        for tool in ('ffmpeg','ffprobe'):
            row,edit=browse_field(self.values.get('paths',{}).get('tools',{}).get(tool,''),False);self.fields[tool]=edit;p.addRow(tool+' 程序',row)
        p.addRow(label('两个媒体工具用于解密后的音频检查和音质识别。已填写有效路径时不用重新安装。','hint'))
        p=self._add_page('歌单平台授权（可选）','公开链接先尝试直接读取；遇到登录要求时再补充。也可以导入各平台或 Playlist Out 导出的歌单文件，本步可跳过。')
        self.playlist_auth_selector=QComboBox();self.playlist_auth_stack=QStackedWidget();self.playlist_credentials={}
        for platform,name in PLATFORMS.items():
            if platform=='netease':continue
            self.playlist_auth_selector.addItem(name,platform);panel=QWidget();form=QVBoxLayout(panel);form.setContentsMargins(0,12,0,0);form.setSpacing(16)
            form.addWidget(label(PLAYLIST_AUTH_HELP[platform],'hint'))
            settings=self.values.get('playlist_sources',{}).get(platform,{})
            edit=CredentialField('Cookie JSON / Token / API Key，格式见上方说明',settings.get('credential_ref') in self.saved_refs)
            self.playlist_credentials[platform]=edit;form.addWidget(edit)
            def load_credential(field=edit):
                path=QFileDialog.getOpenFileName(self,'导入平台登录信息','','凭据 (*.json *.txt);;所有文件 (*)')[0]
                if not path:return
                try:
                    file=Path(path)
                    if file.stat().st_size>1_000_000:raise ValueError()
                    field.setText(file.read_text('utf-8-sig').strip())
                except (OSError,UnicodeError,ValueError):QMessageBox.information(self,'无法导入','请选择不超过 1MB 的 UTF-8 登录信息文件。')
            form.addWidget(button('从文件导入登录信息',load_credential));form.addStretch();self.playlist_auth_stack.addWidget(panel)
        self.playlist_auth_selector.currentIndexChanged.connect(self.playlist_auth_stack.setCurrentIndex);p.addRow('选择需要授权的平台',self.playlist_auth_selector);p.addRow(self.playlist_auth_stack)
        p.addRow(label('登录信息只保存在本机并绑定到该平台官方 API 域名，不发送给第三方聚合解析服务。','hint'))
        p=self._add_page('补全封面、歌词与资料','这些服务都可以跳过。自动写入歌词时保存为音频同名的 .lrc 文件；没有歌词的曲目不会被视为损坏。')
        lyrics=self.values.get('lyrics',{}).get('primary',{});self.lyric_url=line('' if '.example.invalid' in lyrics.get('base_url','') else lyrics.get('base_url',''),'歌词与封面接口地址');self.lyric_key=CredentialField('填写服务提供的完整 Authorization 值',lyrics.get('auth_secret_ref') in self.saved_refs)
        p.addRow('歌词接口地址',self.lyric_url);p.addRow('接口鉴权值',self.lyric_key);p.addRow(label('这里填服务给你的鉴权原文，无需填写“凭据引用”。没有服务可留空。','hint'))
        self.discogs=CredentialField('Discogs 设置中生成的个人 Token；可跳过',self.values.get('catalog',{}).get('discogs',{}).get('credential_ref') in self.saved_refs);p.addRow('Discogs Token',self.discogs)
        ai=self.values.get('ai',{});self.ai_enabled=QCheckBox('使用 AI 辅助解释待核对项目');self.ai_enabled.setChecked(ai.get('enabled',False));p.addRow(self.ai_enabled)
        self.ai_url=line('' if '.example.invalid' in ai.get('base_url','') else ai.get('base_url',''),'兼容 Responses 的 API 地址');self.ai_model=line(ai.get('model',''),'服务提供的模型名称');self.ai_key=CredentialField('API Key',ai.get('auth_secret_ref') in self.saved_refs)
        p.addRow('AI 地址',self.ai_url);p.addRow('模型名称',self.ai_model);p.addRow('API Key',self.ai_key)
        p=self._add_page('连接 Navidrome（可选）','程序完成入库后会触发 Navidrome 扫描。若只使用 Roon，可直接保存。')
        nav=self.values.get('music_servers',{}).get('navidrome',{});self.nav_enabled=QCheckBox('启用 Navidrome');self.nav_enabled.setChecked(nav.get('enabled',False));p.addRow(self.nav_enabled)
        self.nav_url=line(nav.get('url',''),'例如 http://127.0.0.1:4533');self.nav_user=line(nav.get('username',''),'具有扫描权限的账号');self.nav_pass=CredentialField('Navidrome 账号密码',nav.get('password_ref') in self.saved_refs)
        p.addRow('服务地址',self.nav_url);p.addRow('用户名',self.nav_user);p.addRow('密码',self.nav_pass)
        p.addRow(label('Navidrome 需要监听与你选择的音乐库相同的文件。密码只在当前 Windows 账户的本地保险库中保存。','hint'))
        self.save_note=label('');p.addRow(self.save_note)
    def _add_page(self,title,subtitle):
        page=QWizardPage();page.setTitle(title);page.setSubTitle(subtitle);outer=QVBoxLayout(page);scroll=QScrollArea();scroll.setWidgetResizable(True);scroll.setFrameShape(QScrollArea.NoFrame);body=QWidget();form=QFormLayout(body);form.setVerticalSpacing(16);form.setHorizontalSpacing(22);form.setFieldGrowthPolicy(QFormLayout.ExpandingFieldsGrow);scroll.setWidget(body);outer.addWidget(scroll);self.addPage(page);return form
    def showEvent(self,event):
        super().showEvent(event);QTimer.singleShot(0,lambda:apply_native_titlebar(self))
    def accept(self):
        payload={'library':self.library.text().strip(),'netease_cookie_json':self.netease_cookie.toPlainText().strip(),'sites':{k:{'enabled':e.isChecked(),'cookie_json':t.toPlainText().strip()} for k,(e,t) in self.site_fields.items()},
                 'playlist_credentials':{key:edit.text() for key,edit in self.playlist_credentials.items()},
                 'qbt':{'url':self.qbt_url.text().strip(),'username':self.qbt_user.text().strip(),'password':self.qbt_pass.text()},
                 'lyrics':{'url':self.lyric_url.text().strip(),'authorization':self.lyric_key.text()},'discogs_token':self.discogs.text(),
                 'ai':{'enabled':self.ai_enabled.isChecked(),'url':self.ai_url.text().strip(),'model':self.ai_model.text().strip(),'key':self.ai_key.text()},
                 'servers':{'roon':self.roon.isChecked(),'navidrome':{'enabled':self.nav_enabled.isChecked(),'url':self.nav_url.text().strip(),'username':self.nav_user.text().strip(),'password':self.nav_pass.text()}},
                 'workflow':{'netease_fallback':self.fallback.isChecked(),'netease_api_url':self.netease_api.text().strip(),'netease_import_dir':self.import_dir.text().strip(),'fl_per_batch':self.fl_limit.value(),'fl_unlimited':self.fl_unlimited.isChecked()},
                 **{k:w.text().strip() for k,w in self.fields.items()}}
        self.button(QWizard.FinishButton).setEnabled(False);self.save_note.setText('正在保存…')
        def saved(result):self.owner.config=result['config']['values'];super(SetupWizard,self).accept();self.owner.refresh()
        def failed(message):self.button(QWizard.FinishButton).setEnabled(True);self.save_note.setText(message)
        self.owner.call('save_setup',payload,saved,failed)

class Workbench(QMainWindow):
    def __init__(self,base,client=None,autostart=True):
        super().__init__();self.base=Path(base);self.client=client or WorkerClient(base);self.config={};self.tasks={};self.jobs={};self.targets=[];self.requests=set();self.task_polls=set();self.pool=QThreadPool(self);self.setWindowTitle('MusicOps · 音乐库工作台');self.resize(1260,860);self.setMinimumSize(1040,740);self.setWindowIcon(app_icon())
        if QApplication.instance():QApplication.instance().setWindowIcon(app_icon())
        self.setStyleSheet(STYLE);self.setPalette(dark_palette())
        container=QWidget();self.setCentralWidget(container);layout=QHBoxLayout(container);layout.setContentsMargins(0,0,0,0);layout.setSpacing(0)
        side=QWidget();side.setObjectName('sidebar');side.setFixedWidth(224);s=QVBoxLayout(side);s.setContentsMargins(22,28,20,24)
        logo=QLabel();logo.setObjectName('brandMark');logo.setFixedSize(58,58);logo.setPixmap(app_icon().pixmap(58,58));s.addWidget(logo);s.addSpacing(8);s.addWidget(label('MusicOps','brand'));s.addWidget(label('每一张专辑，井然有序','muted'));s.addSpacing(30);s.addWidget(label('工作台','sectionCaption'))
        self.nav=QListWidget();self.nav.addItems(['歌单入库','资料补全','音质升级','任务中心','专辑搜索']);self.nav.setSpacing(4);self.nav.setIconSize(QSize(22,22))
        for index in range(self.nav.count()):self.nav.item(index).setIcon(nav_icon(index))
        s.addWidget(self.nav);s.addWidget(button('连接与设置',self.setup));s.addSpacing(14);s.addWidget(label('你的音乐 · 你的收藏','sidebarFoot'));layout.addWidget(side)
        content=QWidget();content.setObjectName('workspace');c=QVBoxLayout(content);c.setContentsMargins(32,23,32,24);c.setSpacing(16);top=QHBoxLayout();self.connection=label('正在连接后台…','connection');top.addWidget(self.connection,1);self.activity=label('正在连接','activity');top.addWidget(self.activity);c.addLayout(top)
        self.busy_bar=QProgressBar();self.busy_bar.setObjectName('activityProgress');self.busy_bar.setRange(0,0);self.busy_bar.setTextVisible(False);self.busy_bar.setFixedHeight(3);c.addWidget(self.busy_bar)
        self.notice=label('','notice');self.notice.setVisible(False)
        c.addWidget(self.notice);self.stack=QStackedWidget();c.addWidget(self.stack);layout.addWidget(content);self.nav.currentRowChanged.connect(self.switch_page)
        self._playlist();self._complete();self._upgrade();self._tasks();self._album_search();self.nav.setCurrentRow(0)
        self.timer=QTimer(self);self.timer.setInterval(2000);self.timer.timeout.connect(self.refresh);self.wizard_shown=False;self.job_offset=0;self.job_total=0;self._page_animation=None;self.update_activity()
        if autostart:self.call('attach',{},lambda r:self.call('enable_online',{},lambda _:self.refresh()))
    def showEvent(self,event):
        super().showEvent(event);QTimer.singleShot(0,lambda:apply_native_titlebar(self))
    def switch_page(self,index):
        if index<0:return
        if getattr(self,'_page_animation',None):self._page_animation.stop()
        self.stack.setCurrentIndex(index);page=self.stack.widget(index)
        if page is None:return
        effect=page.graphicsEffect()
        if effect is None:effect=QGraphicsOpacityEffect(page);page.setGraphicsEffect(effect)
        effect.setOpacity(.35);self._page_animation=QPropertyAnimation(effect,b'opacity',self);self._page_animation.setDuration(180);self._page_animation.setStartValue(.35);self._page_animation.setEndValue(1);self._page_animation.setEasingCurve(QEasingCurve.OutCubic);self._page_animation.start()
    def update_activity(self):
        busy=bool(self.requests or self.tasks or getattr(self,'active_jobs',0));self.activity.setText('●  正在处理' if busy else '●  后台就绪');self.busy_bar.setRange(0,0) if busy else self.busy_bar.setRange(0,100);self.busy_bar.setValue(0 if busy else 100);self.notice.setVisible(bool(self.notice.text()))
    def page(self,title,subtitle):
        w=QWidget();box=QVBoxLayout(w);box.setContentsMargins(0,0,0,0);box.setSpacing(18);hero=QWidget();hero.setObjectName('hero');hero_box=QVBoxLayout(hero);hero_box.setContentsMargins(24,22,24,22);hero_box.setSpacing(9);hero_box.addWidget(label(('COLLECTION','METADATA','SOUND QUALITY','ACTIVITY','DISCOVERY')[self.stack.count()],'eyebrow'));hero_box.addWidget(label(title,'title'));hero_box.addWidget(label(subtitle,'subtitle'));box.addWidget(hero);self.stack.addWidget(w);return box
    def steps(self,box,text):
        group=QGroupBox();group.setObjectName('card');layout=QVBoxLayout(group);layout.addWidget(label(text,'steps'));box.addWidget(group)
    def table(self,columns):
        table=QTableWidget(0,len(columns));table.setHorizontalHeaderLabels(columns);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch);table.horizontalHeader().setDefaultAlignment(Qt.AlignLeft|Qt.AlignVCenter);table.verticalHeader().hide();table.verticalHeader().setDefaultSectionSize(46);table.setAlternatingRowColors(True);table.setSelectionBehavior(QTableWidget.SelectRows);table.setSelectionMode(QAbstractItemView.SingleSelection);table.setEditTriggers(QTableWidget.NoEditTriggers);table.setShowGrid(False)
        if columns[0]=='选择':table.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents)
        return table
    def _playlist(self):
        box=self.page('把歌单带进音乐库','粘贴主流平台的歌单链接或导入歌单文件，自动查找缺少的音乐、整理资料，并交给 Roon / Navidrome。');self.steps(box,'解析歌单  →  四站检索  →  下载  →  补齐资料与校验  →  入库')
        row=QHBoxLayout();self.playlist_platform=QComboBox();self.playlist_platform.addItem('自动识别平台','auto')
        for key,name in PLATFORMS.items():self.playlist_platform.addItem(name,key)
        self.playlist_platform.setMaximumWidth(170);row.addWidget(self.playlist_platform);self.playlist=line('','粘贴歌单链接 / 分享文本；纯 ID 请先选择平台');row.addWidget(self.playlist);row.addWidget(button('解析歌单',self.parse,True));row.addWidget(button('导入文件',self.import_playlist));box.addLayout(row)
        self.playlist_note=label('已在音乐库中的曲目会跳过；找不到的音源可回退网易云。','hint');box.addWidget(self.playlist_note)
        self.track_table=self.table(['选择','歌曲','艺人','专辑','状态']);box.addWidget(self.track_table)
        row=QHBoxLayout();row.addWidget(button('全选 / 取消',self.select_all));row.addStretch();row.addWidget(button('开始入库',lambda:self.start('playlist'),True));box.addLayout(row)
    def _complete(self):
        box=self.page('补齐音乐库资料','检查专辑、艺人、封面和歌词。可靠匹配自动补齐，资料不明确的项目单独列出。');self.steps(box,'读取标签  →  识别缺失资料  →  查询与匹配  →  写入副本并验证')
        card=QGroupBox('将补齐的内容');card.setObjectName('card');form=QVBoxLayout(card)
        for text in ('专辑名、艺人和专辑艺人','封面图片','同名 .lrc 歌词（有匹配时）','多碟编号与目录结构'):
            form.addWidget(label('✓  '+text))
        box.addWidget(card);box.addWidget(label('保留已有可靠资料；有冲突时等待核对。暂无资料的结果缓存一天，避免重复查询。','hint'));box.addStretch();row=QHBoxLayout();row.addWidget(button('重新查询暂无资料',lambda:self.start('complete',True)));row.addStretch();row.addWidget(button('检查并自动补全',lambda:self.start('complete'),True));box.addLayout(row)
    def _upgrade(self):
        box=self.page('寻找更好的音源','比较同一录音、同一发行的音源，验证新版完整后，再将旧版移出音乐库。');self.steps(box,'识别现有音质  →  四站寻找更好版本  →  下载与补齐  →  完整验证  →  替换旧版')
        card=QGroupBox('升级偏好');card.setObjectName('card');form=QVBoxLayout(card);self.prefer24=QCheckBox('同一录音优先选择无损 24 bit');self.prefer24.setChecked(True);form.addWidget(self.prefer24);form.addWidget(label('有损格式比较有效码率；无损格式比较位深、采样率和实际编码，不按文件大小判断。','hint'));box.addWidget(card)
        box.addWidget(label('替换前核对曲目、发行和全部音频。旧版移出音乐库后保留短期恢复副本；下载与做种源不改动。','hint'));box.addStretch();box.addWidget(button('检查并自动升级',lambda:self.start('upgrade'),True))
    def _tasks(self):
        box=self.page('任务中心','下载、资料处理和入库进度在这里查看。关闭窗口后后台继续工作。');row=QHBoxLayout();row.addWidget(button('暂停',lambda:self.call('pause_pipeline',{},lambda _:self.refresh())));row.addWidget(button('继续',lambda:self.call('resume_pipeline',{},lambda _:self.refresh())));row.addWidget(button('核对 / 重试选中任务',self.review));row.addStretch();row.addWidget(button('取消选中任务',self.cancel));box.addLayout(row)
        self.job_table=self.table(['任务','内容','当前步骤','进度','结果']);header=self.job_table.horizontalHeader()
        for column in (0,2,3):header.setSectionResizeMode(column,QHeaderView.ResizeToContents)
        header.setSectionResizeMode(1,QHeaderView.Interactive);self.job_table.setColumnWidth(1,220);header.setSectionResizeMode(4,QHeaderView.Stretch)
        box.addWidget(self.job_table);self.job_detail=label('选择一项查看结果或需要补充的信息。','hint');self.job_detail.setMinimumHeight(42);box.addWidget(self.job_detail);self.job_table.itemSelectionChanged.connect(self.show_detail)
        actions=QHBoxLayout();actions.addWidget(button('AI 解释选中问题',self.explain));actions.addWidget(button('恢复选中任务的原版',self.restore));actions.addStretch();box.addLayout(actions)
        pages=QHBoxLayout();self.job_counts=label('');self.job_counts.setWordWrap(False);pages.addWidget(self.job_counts);pages.addStretch();pages.addWidget(button('上一页',lambda:self.job_page(-200)));pages.addWidget(button('下一页',lambda:self.job_page(200)));box.addLayout(pages)
    def setup(self):
        if not self.config:return
        self.wizard=SetupWizard(self);self.wizard.open()
    def _album_search(self):
        box=self.page('手动寻找一张专辑','同时检索 OpenCD、DicMusic、Orpheus 和 RED。已配置并启用的站点会参与检索。')
        modes=QHBoxLayout();self.album_literal=QRadioButton('原样搜索输入内容');self.album_precise=QRadioButton('识别专辑后精准搜索');self.album_literal.setChecked(True);modes.addWidget(self.album_literal);modes.addWidget(self.album_precise);modes.addStretch();box.addLayout(modes)
        row=QHBoxLayout();self.album_query=line('','输入专辑名，或 艺人 - 专辑名');row.addWidget(self.album_query,2);self.album_artist=line('','艺人（精准识别时可选）');self.album_artist.setEnabled(False);self.album_precise.toggled.connect(self.album_artist.setEnabled);row.addWidget(self.album_artist,1);row.addWidget(button('搜索四站',self.search_album,True));box.addLayout(row)
        self.album_query.setMaxLength(300)
        self.album_note=label('原样模式保留输入内容（最多 300 字符）。精准模式先列出专辑资料，由你确认对应发行，再按规范艺人和专辑名检索。','hint');box.addWidget(self.album_note)
        self.album_results=self.table(['站点','专辑 / 资源','艺人','音质','做种','匹配']);self.album_results.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents);self.album_results.horizontalHeader().setSectionResizeMode(4,QHeaderView.ResizeToContents);box.addWidget(self.album_results)
        row=QHBoxLayout();row.addWidget(label('结果显示站点当前返回页；更多资源可以在站点查看。','hint'));row.addStretch();row.addWidget(button('打开选中资源详情',self.open_album_result));box.addLayout(row);self.manual_candidates={}
    def search_album(self):
        query=self.album_query.text()
        if not query.strip():self.album_note.setText('请先输入要检索的专辑或内容。');return
        self.album_results.setRowCount(0);self.manual_candidates={};self.album_note.setText('检索已提交；后台有其他任务时会排队，请稍候…')
        payload={'mode':'precise' if self.album_precise.isChecked() else 'literal','query':query,'artist':self.album_artist.text()}
        def received(result):
            if result.get('error'):self.album_note.setText('搜索未完成：'+str(result['error']));return
            if result.get('status')=='NEEDS_CHOICE':
                albums=result.get('albums',[]);texts=[str(c.get('album') or c.get('title'))+' · '+' / '.join(c.get('artists',[]))+' · '+str(c.get('date') or c.get('year') or '')+' · '+c.get('source','') for c in albums]
                value,ok=QInputDialog.getItem(self,'确认对应专辑 / 发行','请选择你要精准检索的专辑；多个发行不会自动合并。',texts,0,False)
                if not ok:self.album_note.setText('尚未选择专辑，可以调整搜索内容或使用原样搜索。');return
                self.album_note.setText('正在核对专辑资料并精准检索四站…');self.call('manual_album_search',{**payload,'candidate_id':albums[texts.index(value)]['candidate_id']},received);return
            outcomes=result.get('source_outcomes',{});names={key:name for key,name,_ in SITES};states={'OK':'有结果','NO_RESOURCE':'无资源','DISABLED':'未启用','AUTH_FAILED':'登录失效','AUTH_REQUIRED':'需登录','RATE_LIMITED':'限流','ACCESS_BLOCKED':'访问未通过'}
            states.update(NETWORK_ERROR='网络连接失败',TRANSIENT_ERROR='站点暂时不可用',PARSE_ERROR='页面内容未识别',INVALID_INPUT='检索词格式无效',API_CONTRACT_ERROR='接口返回异常',INVALID_PAYLOAD='返回内容无效',POLICY_BLOCKED='访问已暂停',INPUT_TOO_LARGE='检索词过长',SNAPSHOT_INCOMPLETE='结果尚不完整',FAILED='检索失败')
            summary='；'.join(names.get(key,key)+'：'+states.get(value.get('status'),value.get('status','未知')) for key,value in outcomes.items())
            self.album_note.setText(result.get('message','')+(' 检索词：'+result['query'] if result.get('query') else '')+('\n'+summary if summary else ''))
            rows=result.get('candidates',[]);self.album_results.setRowCount(len(rows))
            for i,candidate in enumerate(rows):
                self.manual_candidates[candidate['candidate_id']]=candidate;media=candidate.get('media',{});quality=' '.join(str(media.get(k) or '') for k in ('format','encoding','media')).strip()
                values=(names.get(candidate['source'],candidate['source']),candidate.get('title',''),' / '.join(candidate.get('artists',[])),quality,str(candidate.get('seeders') or 0),'精准匹配' if candidate.get('precise_match') else '原样结果')
                for j,text in enumerate(values):item=QTableWidgetItem(text);item.setData(Qt.UserRole,candidate['candidate_id']);item.setToolTip(text);self.album_results.setItem(i,j,item)
            if rows:self.album_results.selectRow(0)
        self.call('manual_album_search',payload,received,lambda error:self.album_note.setText('搜索未完成：'+error))
    def open_album_result(self):
        row=self.album_results.currentRow()
        if row<0:return
        candidate=self.manual_candidates.get(self.album_results.item(row,0).data(Qt.UserRole))
        if not candidate:return
        rid=str(candidate.get('resource_id',''));site=candidate['source'];gid=str(candidate.get('group_id',''))
        if not rid.isdigit():return
        host=next((host for key,_,host in SITES if key==site),None)
        if not host:return
        path='/details.php?id='+rid if site=='open.cd' else '/torrents.php?id='+(gid if gid.isdigit() else rid)+'&torrentid='+rid
        QDesktopServices.openUrl(QUrl('https://'+host+path))
    def call(self,command,payload,done=None,failed=None):
        request=Request(self.client,command,payload);self.requests.add(request);self.update_activity()
        def success(result):
            self.requests.discard(request)
            if result.get('accepted') and done:
                self.tasks[result['task_id']]=done;self.timer.start();QTimer.singleShot(0,self.poll_tasks)
            elif done:done(result)
            self.update_activity()
        def failure(message):
            self.requests.discard(request)
            if failed:failed(message)
            else:
                self.notice.setText('需要处理：'+message)
                if command in ('attach','get_status','list_workflows','get_task') and (message.startswith(('[Errno ','[WinError ')) or 'IPC_CONNECTION_ERROR' in message or '后台响应超时' in message):self._connection_notice=self.notice.text()
            self.update_activity()
        request.signals.done.connect(success);request.signals.failed.connect(failure);self.pool.start(request)
    def poll_tasks(self):
        # A compact status only contains recent tasks. Follow accepted task IDs
        # directly so a long library scan cannot vanish behind pipeline events.
        available=[tid for tid in self.tasks if tid not in self.task_polls]
        if not available:return
        start=getattr(self,'_poll_cursor',0)%len(available);ordered=available[start:]+available[:start];self._poll_cursor=start+4
        for tid in ordered[:max(0,4-len(self.task_polls))]:
            self.task_polls.add(tid)
            def received(response,task_id=tid):
                self.task_polls.discard(task_id);task=response.get('task',{})
                if task.get('state') not in ('completed','failed','blocked','cancelled','recovery_required'):return
                callback=self.tasks.pop(task_id,None)
                result=task.get('result') or {'error':task.get('error','任务未完成')}
                if task.get('state')!='completed':self.notice.setText('任务未完成：'+str(task.get('error') or result.get('reason') or task.get('state')))
                if callback:callback(result)
                self.update_activity()
            def failed(message,task_id=tid):
                self.task_polls.discard(task_id);self.notice.setText('正在重新连接任务：'+message);self.update_activity()
            self.call('get_task',{'task_id':tid},received,failed)
    def refresh(self):
        self.poll_tasks()
        if len(self.requests)>5:return
        self.call('get_status',{'compact':True},self.status)
        self.call('list_workflows',{'offset':self.job_offset},self.show_jobs)
    def status(self,result):
        if self.notice.text()==getattr(self,'_connection_notice',None):self.notice.clear()
        self._connection_notice=None
        self.config=result['config']['values'];self.available_refs=[r['ref'] if isinstance(r,dict) else r for r in result.get('secret_refs',[])];self.timer.start();connected=[name for key,name,_ in SITES if self.config['providers'][key]['enabled'] and self.config['providers'][key]['credential_ref'] in self.available_refs]
        self.connection.setText('音乐库：'+self.config['paths']['library']+'     ·     '+('已配置 '+', '.join(connected) if connected else '尚未连接资源站；可在“连接与设置”添加'))
        if not self.wizard_shown and not self.config['workflow']['setup_done']:
            self.wizard_shown=True;QTimer.singleShot(200,self.setup)
        self.poll_tasks()
    def parse(self):
        self.read_playlist({'playlist_input':self.playlist.text().strip(),'platform':self.playlist_platform.currentData()})
    def import_playlist(self):
        path=QFileDialog.getOpenFileName(self,'导入歌单文件','','歌单 (*.json *.csv *.tsv *.txt *.m3u *.m3u8 *.xlsx);;所有文件 (*)')[0]
        if not path:return
        payload={'file_path':path,'platform':self.playlist_platform.currentData()}
        if Path(path).suffix.lower()=='.txt':
            choice,ok=QInputDialog.getItem(self,'确认文本顺序','每行内容按哪种顺序排列？请选择导出文件实际使用的顺序。',['歌名 - 歌手','歌手 - 歌名'],0,False)
            if not ok:return
            payload['text_order']='title-artist' if choice.startswith('歌名') else 'artist-title'
        self.read_playlist(payload)
    def read_playlist(self,payload):
        self.targets=[];self.track_table.setRowCount(0);self.playlist_note.setText('正在读取歌单…')
        def parsed(result):
            if result.get('error'):self.playlist_note.setText(result['error']);return
            if not result.get('targets') and result.get('complete') is not True:
                self.playlist_note.setText(result.get('reason') or '歌单未读取成功；请检查平台授权，或导入导出的歌单文件。');return
            self.loaded_tracks(result)
            if result.get('reason'):self.playlist_note.setText(self.playlist_note.text()+' '+result['reason'])
        self.call('refresh_playlist',payload,parsed,lambda error:self.playlist_note.setText('读取未完成：'+error))
    def loaded_tracks(self,result):
        self.targets=result['targets'];self.track_table.setRowCount(len(self.targets))
        self.track_table.setUpdatesEnabled(False)
        for i,target in enumerate(self.targets):
            check=QTableWidgetItem();check.setFlags(Qt.ItemIsUserCheckable|Qt.ItemIsEnabled);check.setCheckState(Qt.Unchecked);check.setData(Qt.UserRole,target['target_id']);self.track_table.setItem(i,0,check)
            state='待检查音乐库' if target.get('metadata_complete',True) else '资料缺失，需识别'
            if target.get('source'):state=PLATFORMS.get(target['source'],target['source'])+' · '+state
            for j,value in enumerate((target['raw_title'],' / '.join(target['artists']),target.get('album') or '待识别',state),1):self.track_table.setItem(i,j,QTableWidgetItem(value))
        self.track_table.setUpdatesEnabled(True)
        self.playlist_note.setText(f'已载入 {len(self.targets)} 首；选择需要入库的曲目。任务会按专辑去重。'+(' 来源清单未完全对账，仅处理你明确选择的可见曲目。' if not result.get('complete',True) else ''))
    def select_all(self):
        checked=not all(self.track_table.item(i,0).checkState()==Qt.Checked for i in range(self.track_table.rowCount()))
        for i in range(self.track_table.rowCount()):self.track_table.item(i,0).setCheckState(Qt.Checked if checked else Qt.Unchecked)
    def start(self,scene,refresh_resources=False):
        if not self.config.get('workflow',{}).get('setup_done'):self.setup();return
        targets=[self.track_table.item(i,0).data(Qt.UserRole) for i in range(self.track_table.rowCount()) if self.track_table.item(i,0).checkState()==Qt.Checked] if scene=='playlist' else []
        if scene=='playlist' and not targets:self.playlist_note.setText('请先选择要入库的曲目。');return
        self.notice.setText('任务已提交，正在准备音乐库检查…');self.notice.setVisible(True);self.job_offset=0;self.nav.setCurrentRow(3)
        def started(result):
            if result.get('error'):self.notice.setText('任务未创建：'+result['error'])
            else:self.notice.setText(result.get('message','任务已创建'))
            self.refresh()
        self.call('start_workflow',{'scene':scene,'target_ids':targets,'prefer_24bit':self.prefer24.isChecked(),'refresh_resources':refresh_resources},started)
    def show_jobs(self,result):
        self.job_total=result.get('total',0);self.active_jobs=result.get('active',0);self.job_counts.setText(f"共 {self.job_total} 项 · 运行 {self.active_jobs} · 完成 {result.get('finished',0)} · 第 {self.job_offset//200+1} 页")
        selected=self.selected_job();jobs=result.get('jobs',[]);self.jobs={j['job_id']:j for j in jobs};self.job_table.blockSignals(True);self.job_table.setRowCount(len(jobs))
        for i,job in enumerate(jobs):
            batch=job.get('job_kind')=='batch';progress='扫描中' if batch and job['stage']=='preparing' and not job.get('progress') else str(job.get('progress',0))+'%';values=(SCENES.get(job['scene'],job['scene'])+(' · 批次' if batch else ''),job.get('label','音乐库'),STAGES.get(job['stage'],job['stage']),progress,job.get('message',''))
            for j,value in enumerate(values):
                item=QTableWidgetItem(str(value));item.setData(Qt.UserRole,job['job_id']);item.setToolTip(str(value)+('\n'+job.get('detail','') if j==4 and job.get('detail') else ''))
                if j==2:
                    item.setForeground(QColor('#6ee7d0' if job['stage'] in ('completed','restored') else '#f4c77c' if job['stage'] in ('failed','needs_review') else '#9bbbf4'))
                self.job_table.setItem(i,j,item)
            if job['job_id']==selected:self.job_table.selectRow(i)
        if not selected and jobs:self.job_table.selectRow(next((i for i,job in enumerate(jobs) if job.get('job_kind')=='batch' and job['stage']=='preparing'),0))
        self.job_table.blockSignals(False)
        self.show_detail();self.update_activity()
    def job_page(self,delta):
        self.job_offset=max(0,min(max(0,(self.job_total-1)//200*200),self.job_offset+delta));self.refresh()
    def show_detail(self):
        row=self.job_table.currentRow()
        if row>=0:
            job=self.jobs.get(self.job_table.item(row,0).data(Qt.UserRole),{});self.job_detail.setText(job.get('message','')+'\n'+job.get('detail',''))
    def cancel(self):
        row=self.job_table.currentRow()
        if row>=0:self.call('cancel_workflow',{'job_id':self.job_table.item(row,0).data(Qt.UserRole)},lambda _:self.refresh())
    def selected_job(self):
        row=self.job_table.currentRow()
        return self.job_table.item(row,0).data(Qt.UserRole) if row>=0 and self.job_table.item(row,0) else None
    def explain(self):
        job=self.jobs.get(self.selected_job())
        if not job:return
        if not self.config.get('ai',{}).get('enabled'):self.job_detail.setText('可在“连接与设置”中启用 AI 并填写服务地址、模型和 API Key。');return
        self.call('ai_analyze',{'context':{k:job.get(k) for k in ('scene','stage','label','message','detail')},'instructions':'用中文解释此音乐整理任务为什么等待，给出简短处理步骤。不要确认发行身份，不要生成下载或删除命令。'},lambda result:self.job_detail.setText(str(result.get('answer') or result.get('explanation') or result)))
    def restore(self):
        job_id=self.selected_job()
        if not job_id:return
        if self.jobs.get(job_id,{}).get('job_kind')=='batch':self.job_detail.setText('批次记录不包含已替换文件。请选择具体专辑任务恢复原版。');return
        if QMessageBox.question(self,'恢复原版','恢复此任务保存的原版，并移出本任务新发布的文件？后来被改动的文件会拦截恢复；做种任务保留。正在处理的任务请先取消。')!=QMessageBox.Yes:return
        self.call('restore_workflow',{'job_id':job_id},lambda _:self.refresh())
    def review(self):
        job_id=self.selected_job()
        if job_id:self.call('get_workflow',{'job_id':job_id},lambda result:self.review_dialog(result['job']))
    def review_dialog(self,job):
        if job.get('job_kind')=='batch':
            if job.get('stage') in ('failed','cancelled','needs_review'):
                QMessageBox.information(self,'重新开始任务准备','上次准备已中断，请返回对应入口重新开始。本记录不会自动执行。');self.nav.setCurrentRow({'playlist':0,'complete':1,'upgrade':2}.get(job.get('scene'),3))
            else:QMessageBox.information(self,'批次准备记录','正在准备时可查看扫描进度或取消本批次。准备完成后，请选择具体的专辑任务进行核对。')
            return
        kind=job.get('review_kind');payload={'job_id':job['job_id']}
        if kind=='candidate':
            choices=job.get('choices') or [];texts=[c['source']+' · '+c['title']+' · '+' / '.join(c.get('artists',[]))+' · '+str(c.get('media',{}).get('format') or '')+' '+str(c.get('media',{}).get('encoding') or '') for c in choices]
            value,ok=QInputDialog.getItem(self,'核对发行','确认这是你需要的专辑和版本；仍会检查全部音频。',texts,0,False)
            if not ok:return
            payload['choice']=choices[texts.index(value)]['candidate_id']
        elif kind=='edition':
            if QMessageBox.question(self,'确认发行版本','新版曲目和音频已通过检查，但没有同一发行 ID。请确认录音、混音和发行版本一致；确认后会将旧版移出音乐库并保留恢复副本。')!=QMessageBox.Yes:return
        elif kind=='locks':
            if QMessageBox.question(self,'解锁资料字段','专辑艺人或碟号此前被锁定。确认解锁这些字段，并按已核对的完整专辑资料继续整理？')!=QMessageBox.Yes:return
            payload['choice']='unlock'
        elif kind=='resources':
            value,ok=QInputDialog.getItem(self,'歌词服务未通过','修改连接与设置后可重试；也可以明确跳过本次歌词服务。',['已修改，重试','跳过本次歌词服务'],0,False)
            if not ok:return
            payload['choice']='retry' if value.startswith('已修改') else 'skip'
        elif kind=='cover':
            path=QFileDialog.getOpenFileName(self,'选择已核对的专辑封面','','图片 (*.jpg *.jpeg *.png *.webp)')[0]
            if path:payload['cover_path']=path
            elif QMessageBox.question(self,'暂时跳过封面','确认目前没有可靠封面并继续入库？原封面会保留，之后可以再次运行资料补全。')==QMessageBox.Yes:payload['choice']='skip'
            else:return
        elif kind=='numbering':
            dialog=QDialog(self);dialog.setWindowTitle('核对曲序');dialog.resize(740,520);layout=QVBoxLayout(dialog);layout.addWidget(label('根据专辑的正式曲目表填写每首歌的曲号和碟号。'))
            rows=job.get('prepared') or [];table=QTableWidget(len(rows),3);table.setHorizontalHeaderLabels(['歌曲','曲号','碟号']);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch)
            for i,row in enumerate(rows):
                for j,value in enumerate((row['raw_title'],str(row.get('track','')).split('/')[0],str(row.get('disc','1')).split('/')[0])):table.setItem(i,j,QTableWidgetItem(value))
            layout.addWidget(table);buttons=QDialogButtonBox(QDialogButtonBox.Save|QDialogButtonBox.Cancel);buttons.accepted.connect(dialog.accept);buttons.rejected.connect(dialog.reject);layout.addWidget(buttons)
            if dialog.exec()!=QDialog.Accepted:return
            payload['metadata']=[{'track':table.item(i,1).text(),'disc':table.item(i,2).text()} for i in range(len(rows))]
        elif kind=='metadata':
            dialog=QDialog(self);dialog.setWindowTitle('核对曲目资料');dialog.resize(820,580);layout=QVBoxLayout(dialog);layout.addWidget(label('只填写你已确认的歌曲、艺人和专辑。多位艺人使用“ / ”分隔。'))
            links=QHBoxLayout()
            for link in job.get('lookup_links') or []:links.addWidget(button(link['name'],lambda checked=False,url=link['url']:QDesktopServices.openUrl(QUrl(url))))
            layout.addLayout(links);rows=job.get('prepared') or [];table=QTableWidget(len(rows),4);table.setHorizontalHeaderLabels(['歌曲','艺人','专辑','完整时长（秒）']);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch)
            for i,row in enumerate(rows):
                for j,value in enumerate((row.get('raw_title',''),' / '.join(row.get('artists',[])),row.get('album',''),str(row.get('duration') or ''))):table.setItem(i,j,QTableWidgetItem(value))
            layout.addWidget(table);buttons=QDialogButtonBox(QDialogButtonBox.Save|QDialogButtonBox.Cancel);buttons.accepted.connect(dialog.accept);buttons.rejected.connect(dialog.reject);layout.addWidget(buttons)
            if dialog.exec()!=QDialog.Accepted:return
            payload['metadata']=[{k:table.item(i,j).text() for j,k in enumerate(('title','artist','album','duration'))} for i in range(len(rows))]
        elif kind in ('search','error','interrupted'):
            if QMessageBox.question(self,'继续处理','确认已处理连接或工具问题后继续。已有下载会先核对，不会重复请求未知状态的 FL。')!=QMessageBox.Yes:return
            payload['choice']='retry'
        else:
            QMessageBox.information(self,'需要补充资料',job.get('message','')+'\n'+job.get('detail',''));return
        self.call('review_workflow',payload,lambda _:self.refresh())
    def closeEvent(self,event):self.timer.stop();event.accept()

STYLE='''
QWidget { font-family: "Microsoft YaHei", "PingFang SC", "Segoe UI", "SF Pro Text"; font-size: 13px; color: #e6eef8; }
QMainWindow,QWizard,QDialog,QMessageBox { background: #0d1523; }
QWidget#workspace,QStackedWidget,QWizardPage,QScrollArea,QScrollArea > QWidget > QWidget { background: #0d1523; }
QWidget#sidebar { background: qlineargradient(x1:0,y1:0,x2:0,y2:1,stop:0 #121f33,stop:1 #101929); border-right: 1px solid #24334a; }
QLabel#brand { font-size: 28px; font-weight: 700; letter-spacing: -1px; color: #f2f8ff; }
QLabel#title { font-size: 27px; font-weight: 700; color: #f5f9ff; }
QLabel#eyebrow { font-size: 10px; font-weight: 700; letter-spacing: 2px; color: #66ddc8; }
QLabel#subtitle { color: #a9bad0; font-size: 13px; line-height: 1.6; }
QLabel#muted { color: #92a7c0; font-size: 12px; } QLabel#hint { color: #a4b5ca; }
QLabel#sectionCaption { font-size: 11px; color: #7187a4; padding-left: 12px; }
QLabel#sidebarFoot { font-size: 11px; color: #7389a6; }
QLabel#connection { color: #91a7c2; font-size: 11px; }
QLabel#activity { font-size: 11px; color: #7ae4cf; border: 1px solid #244c50; background: #112d32; padding: 6px 11px; border-radius: 11px; }
QWidget#hero { border: 1px solid #2a405b; border-radius: 16px; background: qlineargradient(x1:0,y1:0,x2:1,y2:1,stop:0 #1b3049,stop:.65 #182a40,stop:1 #163438); }
QLabel#steps { color: #7cdbcb; font-size: 12px; }
QLabel#credentialSaved { color: #72d8c6; font-size: 12px; }
QLabel#notice { color: #bdd7ff; background: #192d49; border: 1px solid #304d71; border-radius: 9px; padding: 11px 14px; }
QGroupBox { border: 1px solid #2b3b52; border-radius: 12px; background: #142034; padding: 18px; margin-top: 14px; }
QGroupBox#card { background: #142034; border-color: #293c54; }
QGroupBox::title { subcontrol-origin: margin; left: 18px; padding: 0 7px; color: #d3e6f5; font-weight: 600; }
QPushButton { background: #1b2b42; border: 1px solid #344961; border-radius: 8px; padding: 10px 16px; color: #d4e3f4; min-height: 18px; }
QPushButton:hover { background: #263b55; border-color: #5d7e96; }
QPushButton:pressed { background: #122238; }
QPushButton:focus { border-color: #63d9ca; }
QPushButton:disabled { background: #152135; color: #64768b; border-color: #26364d; }
QPushButton#primary { background: #5cddc5; color: #0a2427; border: 1px solid #65e5cf; font-weight: 700; }
QPushButton#primary:hover { background: #86edda; border-color: #a4f7e8; }
QPushButton#primary:pressed { background: #3fbea9; }
QPushButton#primary:disabled { background: #244f50; color: #7bada8; border-color: #315e5e; }
QLineEdit,QPlainTextEdit,QSpinBox { background: #101b2d; color: #ecf5ff; border: 1px solid #34465f; border-radius: 8px; padding: 10px 12px; selection-background-color: #256a6b; selection-color: white; min-height: 20px; }
QComboBox { background: #101b2d; color: #ecf5ff; border: 1px solid #34465f; border-radius: 8px; padding: 10px 26px 10px 12px; min-height: 20px; }
QComboBox QAbstractItemView { background: #14243a; color: #ecf5ff; selection-background-color: #24485a; }
QComboBox:focus { border-color: #64dcca; } QRadioButton { color: #c1d4e9; spacing: 9px; padding: 6px 0; }
QLineEdit:hover,QPlainTextEdit:hover,QSpinBox:hover { border-color: #55728e; }
QLineEdit:focus,QPlainTextEdit:focus,QSpinBox:focus { border: 1px solid #64dcca; background: #14243a; }
QLineEdit:disabled,QSpinBox:disabled { color: #64758d; background: #101827; border-color: #26354a; }
QSpinBox::up-button,QSpinBox::down-button { width: 20px; background: #22364e; border: 0; border-radius: 3px; }
QListWidget { background: transparent; border: 0; outline: 0; font-size: 14px; }
QListWidget::item { padding: 15px 12px; border-radius: 9px; color: #a6bad2; border: 1px solid transparent; }
QListWidget::item:hover { background: #1a2c43; color: #eef6ff; }
QListWidget::item:selected { background: #1b3b44; color: #9df2dd; border-color: #2c6062; font-weight: 600; }
QTableWidget { background: #121f32; alternate-background-color: #16253a; border: 1px solid #2b3e56; border-radius: 10px; selection-background-color: #24485a; selection-color: #f0fffb; outline: 0; }
QHeaderView::section { background: #1d3048; color: #9eb7d1; padding: 13px 12px; border: 0; border-bottom: 1px solid #32445d; font-weight: 600; font-size: 12px; }
QTableWidget::item { padding: 10px; border-bottom: 1px solid #1e3045; }
QTableWidget::item:selected { background: #24485a; }
QCheckBox { spacing: 10px; color: #c1d4e9; min-height: 24px; }
QCheckBox::indicator,QTableWidget::indicator { width: 17px; height: 17px; border: 1px solid #617d98; background: #14243a; border-radius: 4px; }
QCheckBox::indicator:checked,QTableWidget::indicator:checked { background: #5cddc5; border: 2px solid #a0f4e4; image: none; }
QCheckBox::indicator:hover,QTableWidget::indicator:hover { border-color: #8be9d7; }
QProgressBar#activityProgress { background: #1d2c41; border: 0; border-radius: 1px; }
QProgressBar#activityProgress::chunk { background: #4dcab9; border-radius: 1px; }
QScrollArea { border: 0; }
QScrollBar:vertical { width: 9px; background: transparent; margin: 3px; }
QScrollBar::handle:vertical { background: #3b526b; min-height: 36px; border-radius: 3px; }
QScrollBar::handle:vertical:hover { background: #5d8199; }
QScrollBar::add-line:vertical,QScrollBar::sub-line:vertical { height: 0; }
QScrollBar::add-page:vertical,QScrollBar::sub-page:vertical { background: transparent; }
QToolTip { color: #e8f6ff; background: #20354c; border: 1px solid #58718b; padding: 7px; }
'''
