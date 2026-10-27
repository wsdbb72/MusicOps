"""Three user workflows and a skippable connection wizard."""
from __future__ import annotations
import copy,json,os,re
from pathlib import Path
from PySide6.QtCore import QObject,QRunnable,QThreadPool,QTimer,Qt,Signal,QSize
from PySide6.QtGui import QFont,QDesktopServices,QIcon,QPixmap,QPainter,QPen,QColor,QPalette
from PySide6.QtCore import QUrl
from PySide6.QtWidgets import (QApplication,QMainWindow,QWidget,QVBoxLayout,QHBoxLayout,QLabel,QPushButton,QLineEdit,QPlainTextEdit,QFileDialog,QCheckBox,QGroupBox,QFormLayout,QWizard,QWizardPage,QListWidget,QStackedWidget,QTableWidget,QTableWidgetItem,QTreeWidget,QTreeWidgetItem,QHeaderView,QMessageBox,QSpinBox,QScrollArea,QInputDialog,QDialog,QDialogButtonBox,QProgressBar,QAbstractItemView,QRadioButton,QTreeWidgetItemIterator)
from musicops.gui.controls import QComboBox
from musicops.gui.layouts import WrapLayout
from musicops import __version__
from musicops.worker.client import WorkerClient
from musicops.clients.http import redact
from musicops.gui.branding import app_icon,apply_native_titlebar
from musicops.providers.playlist_input import PLATFORMS
from musicops.providers.playlist_access import HELP as PLAYLIST_AUTH_HELP

SITES=[('open.cd','OpenCD','www.open.cd'),('dicmusic','DicMusic','dicmusic.com'),('orpheus','Orpheus','orpheus.network'),('redacted','RED','redacted.sh')]
SCENES={'playlist':'歌单入库','complete':'资料补全','upgrade':'音质升级','fill':'补全整张专辑'}
STAGES={'preparing':'检查本地库并准备任务','identifying':'识别歌曲与专辑','snapshotting':'核对原音源','queued':'已排队','restored':'已恢复原版','enrich':'补全资料','planning':'分析曲目','searching':'寻找音源','requesting':'获取下载资源','downloading':'下载中','fallback':'流媒体完整音源回退','metadata':'整理专辑资料','validating':'检查完整音频','publishing':'写入音乐库','retiring':'移出旧版','notifying':'通知音乐服务器','running':'正在处理','completed':'已完成','needs_review':'需要核对','failed':'未完成','cancelled':'已取消','skipped':'已跳过','waiting':'等待处理'}
TASK_GROUPS=(('identify','识别任务'),('search','搜索任务'),('repair','本地资料补全'),('process','下载与入库'))

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
    elif index==5:
        p.drawRoundedRect(2,3,10,12,2,2);p.drawRoundedRect(9,8,11,12,2,2);p.drawLine(11,13,16,13);p.drawLine(14,11,16,13);p.drawLine(14,15,16,13)
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
    done=Signal(object);failed=Signal(str);progress=Signal(str)
class Request(QRunnable):
    def __init__(self,client,command,payload):super().__init__();self.client=client;self.command=command;self.payload=payload;self.signals=Signals()
    def run(self):
        try:self.signals.done.emit((self.client.ensure_running(progress=self.signals.progress.emit) if isinstance(self.client,WorkerClient) else self.client.ensure_running()) if self.command=='attach' else self.client.call(self.command,self.payload))
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

class PlatformOrder(QWidget):
    """Named, checkable platforms; no comma-separated internal codes to memorize."""
    def __init__(self,values,parent=None):
        super().__init__(parent)
        from PySide6.QtWidgets import QListWidget,QListWidgetItem
        from musicops.providers.streaming import NAMES
        self.list=QListWidget();self.list.setMaximumHeight(240);box=QVBoxLayout(self);box.setContentsMargins(0,0,0,0);box.addWidget(self.list)
        for key in dict.fromkeys([*values,*NAMES]):
            item=QListWidgetItem(NAMES[key]);item.setData(Qt.UserRole,key);item.setFlags(item.flags()|Qt.ItemIsUserCheckable);item.setCheckState(Qt.Checked if key in values else Qt.Unchecked);self.list.addItem(item)
        actions=QHBoxLayout();actions.addWidget(button('向上',lambda:self.move(-1)));actions.addWidget(button('向下',lambda:self.move(1)));box.addLayout(actions)
    def move(self,step):
        row=self.list.currentRow();target=row+step
        if row>=0 and 0<=target<self.list.count():
            item=self.list.takeItem(row);self.list.insertItem(target,item);self.list.setCurrentRow(target)
    def text(self):return ','.join(self.list.item(i).data(Qt.UserRole) for i in range(self.list.count()) if self.list.item(i).checkState()==Qt.Checked)

class SetupWizard(QWizard):
    def __init__(self,owner):
        super().__init__(owner);self.owner=owner;self.values=copy.deepcopy(owner.config);self.setWindowTitle('MusicOps · 连接与设置');self.setWindowIcon(app_icon());self.setPalette(dark_palette());self.setWizardStyle(QWizard.ClassicStyle);self.resize(900,760);self.setMinimumSize(760,640)
        self.setButtonText(QWizard.NextButton,'继续');self.setButtonText(QWizard.BackButton,'上一步');self.setButtonText(QWizard.FinishButton,'保存并开始使用');self.setButtonText(QWizard.CancelButton,'稍后设置')
        self.setOption(QWizard.HaveCustomButton1,True);self.setButtonText(QWizard.CustomButton1,'跳过本步');self.setOption(QWizard.HaveCustomButton2,True);self.setButtonText(QWizard.CustomButton2,'保存设置');self.customButtonClicked.connect(lambda which:self.accept() if which==QWizard.CustomButton2.value or self.currentId()==self.pageIds()[-1] else self.next())
        self.fields={};self.site_fields={};self.saved_refs=set(getattr(owner,'available_refs',[]))
        p=self._add_page('音乐放在哪里','选择 Roon / Navidrome 读取的音乐库。临时下载、整理和恢复数据放在音乐库同级的 MusicOps 文件夹；入库后清理临时副本。需要持续做种的原始文件保存在 MusicOps/seeding，避免补写标签破坏种子校验。')
        performance=getattr(owner,'performance',{})
        scheduling=('已绑定 '+str(performance.get('efficiency_processors'))+' 个小核' if performance.get('mode')=='efficiency' else '由系统选择处理器')+'；后台使用较低优先级，文件校验最多 4 项并行。'
        p.addRow(label('后台资源：'+scheduling,'hint'))
        p.addRow(button('浏览器验证与内置 FlareSolverr 设置',lambda:self.open_solver_settings()))
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
        p=self._add_page('浏览器验证 · FlareSolverr','程序已附带 FlareSolverr 和浏览器。选择内置服务后无需填写地址或程序路径；遇到站点验证时自动启动。');self.solver_page_id=self.pageIds()[-1]
        solver=self.values.get('browser_solver',{});self.solver_managed=QCheckBox('使用程序内置 FlareSolverr（自动配置与启动）');self.solver_managed.setChecked(solver.get('managed',True));p.addRow(self.solver_managed);self.solver_enabled=QCheckBox('遇到浏览器挑战时使用本机 FlareSolverr');self.solver_enabled.setChecked(solver.get('enabled',False));p.addRow(self.solver_enabled)
        self.solver_url=line(solver.get('base_url','http://127.0.0.1:8191'),'本机 FlareSolverr 地址');p.addRow('验证服务地址',self.solver_url)
        row,self.solver_executable=browse_field(solver.get('executable',''),False);p.addRow('Windows 程序（可选）',row)
        self.solver_auto=QCheckBox('服务未运行时自动启动上述程序');self.solver_auto.setChecked(solver.get('auto_start',False));self.solver_auto.setEnabled(os.name=='nt');p.addRow(self.solver_auto)
        self.solver_note=label('安装包内含验证服务与浏览器，无需另装 Docker 或填写地址。只对检索页的浏览器挑战启用，不重放下载或令牌请求。取消内置选项后可使用已有本机服务。','hint');p.addRow(self.solver_note)
        from musicops.clients.managed_services import engine_root
        engine=engine_root()/'flaresolverr'/('flaresolverr.exe' if os.name=='nt' else 'flaresolverr')
        p.addRow(label('● 内置 FlareSolverr 已随程序安装' if engine.is_file() else '● 当前目录缺少内置组件，请安装完整安装包。','hint'))
        def solver_mode(enabled):
            self.solver_url.setDisabled(enabled)
            self.solver_executable.setDisabled(enabled or os.name!='nt');self.solver_auto.setDisabled(enabled or os.name!='nt')
        self.solver_managed.toggled.connect(solver_mode);solver_mode(self.solver_managed.isChecked())
        self.solver_test_button=button('启动并测试 FlareSolverr',self.test_solver);p.addRow(self.solver_test_button)
        self.solver_test_timer=QTimer(self);self.solver_test_timer.setInterval(1000);self.solver_test_timer.timeout.connect(self.solver_test_tick)
        p=self._add_page('连接 qBittorrent','用于接收站点种子并下载。qBittorrent → 选项 → WebUI，启用 Web 用户界面；把地址与登录账号填在这里。')
        cfg=self.values.get('qbt',{});qbt_saved=cfg.get('credential_ref') in self.saved_refs;self.qbt_url=line(cfg.get('base_url','http://127.0.0.1:8080'),'例如 http://127.0.0.1:8080');self.qbt_user=CredentialField('WebUI 用户名',qbt_saved);self.qbt_pass=CredentialField('WebUI 密码',qbt_saved)
        p.addRow('WebUI 地址',self.qbt_url);p.addRow('用户名',self.qbt_user);p.addRow('密码',self.qbt_pass)
        self.qbt_test_button=button('测试已保存的下载连接',self.test_qbt);p.addRow(self.qbt_test_button);self.qbt_test_note=label('修改地址或账号后先保存。测试只读取连接状态，不添加或启动下载。','hint');p.addRow(self.qbt_test_note)
        self.qbt_tag_button=button('将已有 MusicOps 任务标签统一为 MusicOps',self.normalize_qbt_tags);p.addRow(self.qbt_tag_button)
        p.addRow(label('● 已保存登录信息 · 留空保留；更新时请同时填写用户名与密码。' if qbt_saved else '没有 qBittorrent 时可跳过；资料补全仍可使用。','credentialSaved' if qbt_saved else 'hint'))
        fl_row=QWidget();fl_layout=QHBoxLayout(fl_row);fl_layout.setContentsMargins(0,0,0,0);self.fl_limit=QSpinBox();self.fl_limit.setRange(0,100);self.fl_limit.setValue(self.values.get('workflow',{}).get('fl_per_batch',3));self.fl_limit.setSuffix(' 次 / 批次');self.fl_unlimited=QCheckBox('全部');self.fl_unlimited.setToolTip('不限制本批次使用 FL 的次数；仍仅请求站点确认免费的下载。');self.fl_unlimited.setChecked(self.values.get('workflow',{}).get('fl_unlimited',False));self.fl_limit.setDisabled(self.fl_unlimited.isChecked());self.fl_unlimited.toggled.connect(self.fl_limit.setDisabled);fl_layout.addWidget(self.fl_limit,1);fl_layout.addWidget(self.fl_unlimited);p.addRow('最多使用 FL 免费下载',fl_row)
        p.addRow(label('勾选“全部”后忽略左侧次数，按本批次需要使用 FL。OpenCD 直接获取种子；其他站点仅使用已确认免费的下载，额度不足时等待处理。','hint'))
        self.edition_policy=QComboBox()
        for name,key in (('音质最好优先','quality'),('歌曲最全优先','complete'),('特殊版本优先（豪华 / 周年 / 重制）','special')):self.edition_policy.addItem(name,key)
        self.edition_policy.setCurrentIndex(max(0,self.edition_policy.findData(self.values['workflow'].get('edition_policy','quality'))));p.addRow('发行自动筛选',self.edition_policy)
        self.artist_folders=QCheckBox('资料补全后把根目录下的专辑移到“音乐人/专辑”文件夹');self.artist_folders.setChecked(self.values['workflow'].get('artist_folders',True));self.artist_folders.setToolTip('例如“陈奕迅 - 阴天快乐”整理为“陈奕迅/阴天快乐”；合作专辑用“A／B”命名。已在音乐人文件夹中的专辑不动。');p.addRow(self.artist_folders)
        p=self._add_page('流媒体、媒体工具与歌单授权','四站没有合适音源时，按顺序获取可验证的完整音源。平台 Cookie 与扫码登录统一管理，歌单读取和下载可复用登录态。')
        self.auto_updates=QCheckBox('启动时检查、下载并启动当前系统更新');self.auto_updates.setChecked(self.values.get('updates',{}).get('enabled',True));p.addRow('软件更新',self.auto_updates)
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
        p.addRow(label('多平台兜底 · 按勾选顺序处理，完整音频校验后才会入库。','hint'))
        streaming=self.values.get('streaming',{});self.streaming_managed=QCheckBox('使用程序内置 go-music-dl（自动配置与启动）');self.streaming_managed.setChecked(streaming.get('managed',True));p.addRow(self.streaming_managed);self.streaming_enabled=QCheckBox('启用多平台流媒体兜底');self.streaming_enabled.setChecked(streaming.get('enabled',False));p.addRow(self.streaming_enabled)
        self.streaming_url=line(streaming.get('base_url','http://127.0.0.1:8080/music'),'填写 go-music-dl 网页的完整地址，含 /music');p.addRow('服务网页地址',self.streaming_url)
        self.streaming_cookie=CredentialField('可选：go-music-dl 登录 Cookie JSON，未启用登录时留空',streaming.get('credential_ref') in self.saved_refs);p.addRow('服务登录 Cookie',self.streaming_cookie)
        self.streaming_order=PlatformOrder(streaming.get('order',[]));p.addRow('参与平台与尝试顺序',self.streaming_order)
        def streaming_mode(enabled):
            self.streaming_url.setDisabled(enabled);self.streaming_cookie.setDisabled(enabled)
        self.streaming_managed.toggled.connect(streaming_mode);streaming_mode(self.streaming_managed.isChecked())
        def login():
            from musicops.gui.engine_login import EngineLogin
            dialog=EngineLogin(self.owner,self.saved_refs);dialog.exec();self.saved_refs.update(dialog.saved_refs)
        p.addRow(button('平台 Cookie / 扫码登录…',login))
        external=streaming['musicdl'];self.musicdl_enabled=QCheckBox('启用自行安装的 musicdl（用于 TIDAL / Qobuz / Deezer 等新增平台）');self.musicdl_enabled.setChecked(external['enabled']);self.musicdl_enabled.toggled.connect(lambda checked:self.streaming_enabled.setChecked(True) if checked else None);p.addRow(self.musicdl_enabled)
        row,self.musicdl_python=browse_field(external['python_path'],False);p.addRow('已安装 musicdl 的 Python',row)
        p.addRow(label('musicdl 限制直接捆绑分发，因此由你在独立 Python 环境安装。这里只连接已有安装，仍按歌名、艺人、版本、完整时长和实际解码校验，不接受试听。新增平台需要勾选并排入上方顺序。','hint'))
        self.musicdl_cookies={};external_box=QComboBox();external_stack=QStackedWidget()
        from musicops.providers.musicdl_bridge import CLIENTS
        from musicops.providers.streaming import NAMES
        for platform in CLIENTS:
            external_box.addItem(NAMES[platform],platform);edit=CredentialField('可选：{"cookie_name":"cookie_value"}，使用该平台自己的账号',external['credential_refs'].get(platform) in self.saved_refs);self.musicdl_cookies[platform]=edit;external_stack.addWidget(edit)
        external_box.currentIndexChanged.connect(external_stack.setCurrentIndex);p.addRow('新增平台登录态',external_box);p.addRow(external_stack)

        p.addRow(label('歌单 API 授权 · 公开链接先直接读取；需要 Token 的平台按下面说明填写。Cookie 与扫码登录在上方统一管理。','hint'))
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
        p.addRow('歌词接口地址',self.lyric_url);p.addRow('接口鉴权值',self.lyric_key);p.addRow(button('测试当前填写的歌词接口',lambda:self.test_connection('lyrics')));self.lyric_test_note=label('测试不写入音乐，也不保存新凭据。','hint');p.addRow(self.lyric_test_note);p.addRow(label('这里填服务给你的鉴权原文，无需填写“凭据引用”。没有服务可留空。','hint'))
        self.discogs=CredentialField('Discogs 设置中生成的个人 Token；可跳过',self.values.get('catalog',{}).get('discogs',{}).get('credential_ref') in self.saved_refs);p.addRow('Discogs Token',self.discogs)
        self.catalog_web=QCheckBox('API 无匹配时自动读取公开音乐资料网站');self.catalog_web.setChecked(self.values.get('catalog',{}).get('web_enabled',True));p.addRow(self.catalog_web)
        self.prefer_simplified=QCheckBox('已确认中文的资料优先简体；日文和语言不明的名称保持原文');self.prefer_simplified.setChecked(self.values.get('metadata',{}).get('prefer_simplified',True));p.addRow(self.prefer_simplified)
        ai=self.values.get('ai',{});self.ai_enabled=QCheckBox('启用 AI：API 和网站均无法匹配时联网搜索，并辅助核对');self.ai_enabled.setChecked(ai.get('enabled',False));p.addRow(self.ai_enabled)
        self.ai_url=line('' if '.example.invalid' in ai.get('base_url','') else ai.get('base_url',''),'兼容 Responses 的 API 地址');self.ai_model=line(ai.get('model',''),'服务提供的模型名称');self.ai_key=CredentialField('API Key',ai.get('auth_secret_ref') in self.saved_refs)
        self.ai_key.textChanged.connect(lambda value:self.ai_enabled.setChecked(True) if value.strip() else None)
        p.addRow('AI 地址',self.ai_url);p.addRow('模型名称',self.ai_model);p.addRow('API Key',self.ai_key)
        p.addRow(button('测试当前 AI 地址、密钥与模型',lambda:self.test_connection('ai')));self.ai_test_note=label('连接测试仅发送一条简短请求。','hint');p.addRow(self.ai_test_note)
        p.addRow(label('顺序：识别 API → 公开资料网站 → AI 联网搜索。AI 地址和模型必须支持 Responses web_search；不支持时会显示原因并等待手动核对，不会伪装联网成功。网站限制访问时可打开来源人工核对。','hint'))
        p=self._add_page('连接 Plex / Navidrome（可选）','程序完成入库后会通知已启用的音乐服务扫描。若只使用 Roon，可直接保存。')
        nav=self.values.get('music_servers',{}).get('navidrome',{});self.nav_enabled=QCheckBox('启用 Navidrome');self.nav_enabled.setChecked(nav.get('enabled',False));p.addRow(self.nav_enabled)
        self.nav_url=line(nav.get('url',''),'例如 http://127.0.0.1:4533');self.nav_user=line(nav.get('username',''),'具有扫描权限的账号');self.nav_pass=CredentialField('Navidrome 账号密码',nav.get('password_ref') in self.saved_refs)
        p.addRow('服务地址',self.nav_url);p.addRow('用户名',self.nav_user);p.addRow('密码',self.nav_pass)
        p.addRow(label('Navidrome 需要监听与你选择的音乐库相同的文件。密码只在当前 Windows 账户的本地保险库中保存。','hint'))
        plex=self.values['music_servers']['plex'];self.plex_enabled=QCheckBox('入库后通知 Plex 扫描音乐库');self.plex_enabled.setChecked(plex['enabled']);p.addRow(self.plex_enabled)
        self.plex_url=line(plex['url'],'例如 http://192.168.1.10:32400');self.plex_section=line(plex['section_id'],'测试读取音乐库后填写对应 ID');self.plex_token=CredentialField('X-Plex-Token',plex['auth_secret_ref'] in self.saved_refs)
        p.addRow('Plex 地址',self.plex_url);p.addRow('Plex 音乐库 ID',self.plex_section);p.addRow('Plex Token',self.plex_token);p.addRow(button('测试 Plex 并读取音乐库 ID',lambda:self.test_connection('plex')));self.plex_test_note=label('仅列出音乐库，不触发扫描。','hint');p.addRow(self.plex_test_note)
        self.save_note=label('');p.addRow(self.save_note)
    def test_connection(self,kind):
        if kind=='lyrics':settings={'base_url':self.lyric_url.text().strip(),'authorization':self.lyric_key.text()};note=self.lyric_test_note
        elif kind=='ai':settings={'base_url':self.ai_url.text().strip(),'model':self.ai_model.text().strip(),'key':self.ai_key.text()};note=self.ai_test_note
        else:settings={'url':self.plex_url.text().strip(),'token':self.plex_token.text()};note=self.plex_test_note
        note.setText('正在测试当前填写的连接…')
        def done(result):
            note.setText(result.get('message',result.get('status','测试完成'))+'；耗时 '+str(result.get('seconds',0))+' 秒'+('\n'+'\n'.join(row['id']+' · '+str(row['name']) for row in result.get('sections',[])) if result.get('sections') else ''))
        self.owner.call('test_connection',{'kind':kind,'settings':settings},done,lambda message:note.setText('测试未通过：'+message))
    def _add_page(self,title,subtitle):
        page=QWizardPage();page.setTitle(title);page.setSubTitle(subtitle);outer=QVBoxLayout(page);scroll=QScrollArea();scroll.setWidgetResizable(True);scroll.setFrameShape(QScrollArea.NoFrame);body=QWidget();form=QFormLayout(body);form.setVerticalSpacing(16);form.setHorizontalSpacing(22);form.setFieldGrowthPolicy(QFormLayout.ExpandingFieldsGrow);scroll.setWidget(body);outer.addWidget(scroll);self.addPage(page);return form
    def test_solver(self):
        if self.solver_test_timer.isActive():return
        self.solver_test_seconds=0;self.solver_test_button.setEnabled(False);self.solver_test_timer.start();self.solver_test_tick()
        self.owner.call('test_browser_solver',{'settings':{'managed':self.solver_managed.isChecked(),'base_url':self.solver_url.text().strip(),'executable':self.solver_executable.text().strip(),'auto_start':self.solver_auto.isChecked()}},lambda result:self.solver_test_finished(result.get('message') or result.get('error') or str(result.get('status'))),self.solver_test_finished)
    def solver_test_tick(self):
        self.solver_test_seconds+=1
        self.solver_note.setText(f'正在启动并连接本机 FlareSolverr · {self.solver_test_seconds} 秒；首次启动最多约 2 分钟，不占用音乐任务队列。')
        if self.solver_test_seconds>=145:self.solver_test_finished('连接超时；请检查内置组件是否完整，或已有本机服务的地址和状态，然后重试。')
    def solver_test_finished(self,message):
        self.solver_test_timer.stop();self.solver_test_button.setEnabled(True);self.solver_note.setText(message)
    def open_solver_settings(self):
        self.setStartId(self.solver_page_id);self.restart()
    def normalize_qbt_tags(self):
        self.qbt_tag_button.setEnabled(False);self.qbt_test_note.setText('已提交标签整理；后台会核实每项任务的归属，不启动下载。')
        def finished(message):self.qbt_tag_button.setEnabled(True);self.qbt_test_note.setText(message)
        self.owner.call('normalize_qbt_tags',{},lambda result:finished(result.get('message') or result.get('error') or '标签整理完成'),finished)
    def test_qbt(self):
        self.qbt_test_button.setEnabled(False);self.qbt_test_note.setText('正在检查已保存的 qBittorrent WebUI 与账号…')
        def finished(message):self.qbt_test_button.setEnabled(True);self.qbt_test_note.setText(message)
        self.owner.call('test_qbt_connection',{},lambda result:finished('下载连接正常，可接收专辑任务。' if result.get('status')=='OK' else result.get('reason') or result.get('error') or '下载连接未通过：'+str(result.get('status'))),finished)
    def showEvent(self,event):
        super().showEvent(event);QTimer.singleShot(0,lambda:apply_native_titlebar(self))
    def accept(self):
        payload={'library':self.library.text().strip(),'netease_cookie_json':self.netease_cookie.toPlainText().strip(),'sites':{k:{'enabled':e.isChecked(),'cookie_json':t.toPlainText().strip()} for k,(e,t) in self.site_fields.items()},
                 'playlist_credentials':{key:edit.text() for key,edit in self.playlist_credentials.items()},
                 'qbt':{'url':self.qbt_url.text().strip(),'username':self.qbt_user.text().strip(),'password':self.qbt_pass.text()},
                 'lyrics':{'url':self.lyric_url.text().strip(),'authorization':self.lyric_key.text()},'discogs_token':self.discogs.text(),
                 'ai':{'enabled':self.ai_enabled.isChecked(),'url':self.ai_url.text().strip(),'model':self.ai_model.text().strip(),'key':self.ai_key.text()},'catalog_web':self.catalog_web.isChecked(),'prefer_simplified':self.prefer_simplified.isChecked(),
                 'servers':{'plex':{'enabled':self.plex_enabled.isChecked(),'url':self.plex_url.text().strip(),'section_id':self.plex_section.text().strip(),'token':self.plex_token.text()},'roon':self.roon.isChecked(),'navidrome':{'enabled':self.nav_enabled.isChecked(),'url':self.nav_url.text().strip(),'username':self.nav_user.text().strip(),'password':self.nav_pass.text()}},
                 'updates':{'enabled':self.auto_updates.isChecked()},'browser_solver':{'managed':self.solver_managed.isChecked(),'enabled':self.solver_enabled.isChecked(),'base_url':self.solver_url.text().strip(),'executable':self.solver_executable.text().strip(),'auto_start':self.solver_auto.isChecked()},
                 'workflow':{'netease_fallback':self.fallback.isChecked(),'netease_api_url':self.netease_api.text().strip(),'netease_import_dir':self.import_dir.text().strip(),'fl_per_batch':self.fl_limit.value(),'fl_unlimited':self.fl_unlimited.isChecked(),'artist_folders':self.artist_folders.isChecked(),'edition_policy':self.edition_policy.currentData()},
                 'streaming':{'musicdl':{'enabled':self.musicdl_enabled.isChecked(),'python_path':self.musicdl_python.text().strip(),'cookies':{p:e.text() for p,e in self.musicdl_cookies.items()}},'managed':self.streaming_managed.isChecked(),'enabled':self.streaming_enabled.isChecked(),'base_url':self.streaming_url.text().strip(),'cookie_json':self.streaming_cookie.text(),'order':[s.strip() for s in self.streaming_order.text().split(',') if s.strip()]},
                 **{k:w.text().strip() for k,w in self.fields.items()}}
        self.button(QWizard.FinishButton).setEnabled(False);self.button(QWizard.CustomButton2).setEnabled(False);self.save_note.setText('正在保存…')
        def saved(result):self.owner.config=result['config']['values'];super(SetupWizard,self).accept();self.owner.refresh()
        def failed(message):self.button(QWizard.FinishButton).setEnabled(True);self.button(QWizard.CustomButton2).setEnabled(True);self.save_note.setText(message);self.currentPage().setSubTitle('保存未完成：'+message)
        self.owner.call('save_setup',payload,saved,failed)

class Workbench(QMainWindow):
    def __init__(self,base,client=None,autostart=True,updates_enabled=False):
        super().__init__();self.updates_enabled=updates_enabled;self.update_checked=False;self.update_controller=None;self.base=Path(base);self.client=client or WorkerClient(base);self.config={};self.tasks={};self.task_failures={};self.jobs={};self.targets=[];self.requests=set();self.task_polls=set();self.pool=QThreadPool(self);self.pool.setMaxThreadCount(8);self.setWindowTitle('MusicOps · 音乐库工作台');self.resize(1260,860);self.setMinimumSize(1040,740);self.setWindowIcon(app_icon())
        if QApplication.instance():QApplication.instance().setWindowIcon(app_icon())
        self.setStyleSheet(STYLE);self.setPalette(dark_palette())
        container=QWidget();self.setCentralWidget(container);layout=QHBoxLayout(container);layout.setContentsMargins(0,0,0,0);layout.setSpacing(0)
        side=QWidget();side.setObjectName('sidebar');side.setFixedWidth(224);s=QVBoxLayout(side);s.setContentsMargins(22,28,20,24)
        logo=QLabel();logo.setObjectName('brandMark');logo.setFixedSize(58,58);logo.setPixmap(app_icon().pixmap(58,58));s.addWidget(logo);s.addSpacing(8);s.addWidget(label('MusicOps','brand'));s.addWidget(label('每一张专辑，井然有序','muted'));s.addSpacing(30);s.addWidget(label('工作台','sectionCaption'))
        self.nav=QListWidget();self.nav.addItems(['歌单入库','资料补全','音质升级','任务中心','歌手 / 专辑搜索','去重合并']);self.nav.setSpacing(4);self.nav.setIconSize(QSize(22,22))
        for index in range(self.nav.count()):self.nav.item(index).setIcon(nav_icon(index))
        s.addWidget(self.nav);s.addWidget(button('连接与设置',self.setup));s.addWidget(button('镜像更新说明' if os.environ.get('MUSICOPS_CONTAINER')=='1' else '检查更新',lambda:self.check_updates(True)));self.update_note=label('','sidebarFoot');self.update_note.setWordWrap(True);s.addWidget(self.update_note);s.addSpacing(14);s.addWidget(label('MusicOps '+__version__+' · 你的音乐收藏','sidebarFoot'));layout.addWidget(side)
        content=QWidget();content.setObjectName('workspace');c=QVBoxLayout(content);c.setContentsMargins(32,23,32,24);c.setSpacing(16);top=QHBoxLayout();self.connection=label('正在连接后台…','connection');top.addWidget(self.connection,1);self.activity=label('正在连接','activity');top.addWidget(self.activity);c.addLayout(top)
        self.busy_bar=QProgressBar();self.busy_bar.setObjectName('activityProgress');self.busy_bar.setRange(0,0);self.busy_bar.setTextVisible(False);self.busy_bar.setFixedHeight(3);c.addWidget(self.busy_bar)
        self.notice=label('','notice');self.notice.setVisible(False)
        c.addWidget(self.notice);self.stack=QStackedWidget();c.addWidget(self.stack);layout.addWidget(content);self.nav.currentRowChanged.connect(self.switch_page)
        self._playlist();self._complete();self._upgrade();self._tasks();self._album_search();self._duplicates();self.nav.setCurrentRow(0)
        self.timer=QTimer(self);self.timer.setInterval(2000);self.timer.timeout.connect(self.refresh);self.wizard_shown=False;self.job_offset=0;self.job_total=0;self._page_animation=None;self.update_activity()
        if autostart:self.call('attach',{},lambda r:self.call('enable_online',{},lambda _:self.refresh()))
    def showEvent(self,event):
        super().showEvent(event);QTimer.singleShot(0,lambda:apply_native_titlebar(self))
    def switch_page(self,index):
        if index<0:return
        # Opacity effects cache child surfaces; interrupted transitions could
        # paint an old page over the task tree. Pages now paint opaquely.
        self.stack.setCurrentIndex(index)
        self.stack.update()
        if index==5 and self.config:self.duplicate_page.load_history()
    def update_activity(self):
        paused=getattr(self,'paused_jobs',0);globally_paused=getattr(self,'pipeline_paused',False);foreground=any(request.command not in ('get_status','list_workflows','get_task') for request in self.requests)
        busy=bool(foreground or (self.tasks or getattr(self,'active_jobs',0)) and not globally_paused and not (paused and not getattr(self,'active_jobs',0)))
        self.activity.setText('●  后台已暂停' if globally_paused else '●  正在处理' if busy else f'●  已暂停 {paused} 项' if paused else '●  后台就绪');self.busy_bar.setRange(0,0) if busy else self.busy_bar.setRange(0,100);self.busy_bar.setValue(0 if busy else 100);self.notice.setVisible(bool(self.notice.text()))
    def page(self,title,subtitle):
        w=QWidget();w.setObjectName('workbenchPage');w.setAutoFillBackground(True);box=QVBoxLayout(w);box.setContentsMargins(0,0,0,0);box.setSpacing(18);hero=QWidget();hero.setObjectName('hero');hero_box=QVBoxLayout(hero);hero_box.setContentsMargins(24,22,24,22);hero_box.setSpacing(9);hero_box.addWidget(label(('歌单入库流程','本地资料整理','音源质量比较','后台任务与进度','手动检索音源','音乐库维护')[self.stack.count()],'eyebrow'));hero_box.addWidget(label(title,'title'));hero_box.addWidget(label(subtitle,'subtitle'));box.addWidget(hero);scroll=QScrollArea();scroll.setObjectName('pageScroll');scroll.setWidgetResizable(True);scroll.setFrameShape(QScrollArea.NoFrame);scroll.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff);scroll.setWidget(w);self.stack.addWidget(scroll);return box
    def steps(self,box,text):
        group=QGroupBox();group.setObjectName('card');layout=QVBoxLayout(group);layout.addWidget(label(text,'steps'));box.addWidget(group)
    def table(self,columns):
        table=QTableWidget(0,len(columns));table.setHorizontalHeaderLabels(columns);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch);table.horizontalHeader().setDefaultAlignment(Qt.AlignLeft|Qt.AlignVCenter);table.verticalHeader().hide();table.verticalHeader().setDefaultSectionSize(46);table.setAlternatingRowColors(True);table.setSelectionBehavior(QTableWidget.SelectRows);table.setSelectionMode(QAbstractItemView.SingleSelection);table.setEditTriggers(QTableWidget.NoEditTriggers);table.setShowGrid(False)
        if columns[0]=='选择':table.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents)
        return table
    def _playlist(self):
        box=self.page('把歌单带进音乐库','粘贴主流平台的歌单链接或导入歌单文件，自动查找缺少的音乐、整理资料，并交给 Roon / Navidrome / Plex。');self.steps(box,'读取歌单  →  对比本地音乐库  →  识别缺失专辑  →  搜索与下载  →  校验入库')
        row=QHBoxLayout();self.playlist_platform=QComboBox();self.playlist_platform.addItem('自动识别平台','auto')
        for key,name in PLATFORMS.items():self.playlist_platform.addItem(name,key)
        self.playlist_platform.setMaximumWidth(170);row.addWidget(self.playlist_platform);self.playlist=line('','粘贴歌单链接 / 分享文本；纯 ID 请先选择平台');row.addWidget(self.playlist);row.addWidget(button('解析歌单',self.parse,True));row.addWidget(button('导入文件',self.import_playlist));box.addLayout(row)
        self.playlist_note=label('已在音乐库中的曲目会跳过；找不到的音源可回退网易云。','hint');box.addWidget(self.playlist_note)
        self.track_table=self.table(['选择','歌曲','艺人','专辑','状态']);self.track_table.setMinimumHeight(220);box.addWidget(self.track_table,1)
        self.playlist_full_album=QCheckBox('单曲发行可选择下载完整专辑（先核对，再下载）');self.playlist_full_album.setToolTip('可选。只对真实目录确认的 Single 查找收录相同录音的完整 Album；由你选择原发行或完整专辑，不会仅凭曲目数量猜测。');box.addWidget(self.playlist_full_album)
        row=QHBoxLayout();row.addWidget(button('全选 / 取消',self.select_all));row.addWidget(button('恢复已解析曲目',self.load_cached_playlist));row.addStretch();row.addWidget(button('开始入库',lambda:self.start('playlist'),True));box.addLayout(row)
        self._subscriptions(box)
    def _subscriptions(self,box):
        card=QGroupBox('订阅监控歌单');card.setObjectName('card');form=QVBoxLayout(card)
        form.addWidget(label('订阅上方填写的歌单后，后台按间隔重新读取；只把新增曲目按专辑自动入库，已在音乐库中的跳过，歌单删除的曲目不会从音乐库移除。','hint'))
        row=QHBoxLayout();self.subscription_hours=QSpinBox();self.subscription_hours.setRange(1,168);self.subscription_hours.setValue(6);self.subscription_hours.setPrefix('每 ');self.subscription_hours.setSuffix(' 小时检查')
        self.subscription_existing=QCheckBox('现有曲目也入库');self.subscription_existing.setToolTip('不勾选时，首次检查只记录现有曲目，之后新增的才入库。')
        row.addWidget(self.subscription_hours);row.addWidget(self.subscription_existing);row.addStretch();row.addWidget(button('订阅当前歌单',self.add_subscription,True));form.addLayout(row)
        self.subscription_table=self.table(['歌单','间隔','上次检查','结果','下次检查']);self.subscription_table.setMinimumHeight(160);form.addWidget(self.subscription_table)
        row=QHBoxLayout();row.addWidget(button('立即检查',self.check_subscription));row.addWidget(button('暂停 / 恢复',self.toggle_subscription));row.addWidget(button('删除订阅',self.delete_subscription));row.addStretch();form.addLayout(row)
        self.subscription_note=label('','hint');form.addWidget(self.subscription_note);box.addWidget(card)
        self.subscription_timer=QTimer(self);self.subscription_timer.setInterval(30000);self.subscription_timer.timeout.connect(self.load_subscriptions);self.subscription_timer.start()
        QTimer.singleShot(0,self.load_subscriptions)
    def _complete(self):
        box=self.page('补齐音乐库资料','检查专辑、艺人、封面和歌词。可靠匹配自动补齐，资料不明确的项目单独列出。');self.steps(box,'读取标签  →  识别缺失资料  →  查询与匹配  →  写入副本并验证')
        card=QGroupBox('将补齐的内容');card.setObjectName('card');form=QVBoxLayout(card)
        for text in ('专辑名、艺人和专辑艺人','封面图片','同名 .lrc 歌词（有匹配时）','多碟编号与目录结构'):
            form.addWidget(label('✓  '+text))
        box.addWidget(card);self.catalog_note=label('本地专辑资料库：尚未建立','hint');box.addWidget(self.catalog_note)
        box.addWidget(label('需要补缺曲？这会核对完整曲目表，再搜索、下载缺失歌曲；已在库中的音频保留。','hint'))
        box.addWidget(button('查找并下载专辑缺失的歌曲',lambda:self.start('fill')))
        catalog_actions=QHBoxLayout();catalog_actions.addWidget(button('更新本地资料库',self.refresh_catalog));catalog_actions.addWidget(button('查看专辑资料库',self.browse_catalog));catalog_actions.addStretch();box.addLayout(catalog_actions)
        box.addWidget(label('下面的“补齐已有歌曲资料”只补标签、封面和歌词，不下载音源。保留可靠资料，有冲突时等待核对。未找到的资料缓存一天。','hint'));box.addStretch();row=QHBoxLayout();row.addWidget(button('重新查询之前未找到的资料',lambda:self.start('complete',True)));row.addStretch();row.addWidget(button('补齐已有歌曲资料',lambda:self.start('complete'),True));box.addLayout(row)
    def _upgrade(self):
        box=self.page('寻找更好的音源','比较同一录音、同一发行的音源，验证新版完整后，再将旧版移出音乐库。');self.steps(box,'识别现有音质  →  四站寻找更好版本  →  下载与补齐  →  完整验证  →  替换旧版')
        card=QGroupBox('升级偏好');card.setObjectName('card');form=QVBoxLayout(card);self.prefer24=QCheckBox('同一录音优先选择无损 24 bit');self.prefer24.setChecked(True);form.addWidget(self.prefer24);form.addWidget(label('有损格式比较有效码率；无损格式比较位深、采样率和实际编码，不按文件大小判断。','hint'));box.addWidget(card)
        box.addWidget(label('单曲也可升级为它收录到的完整专辑：先选择原单曲，再确认完整专辑的年份与版本；允许同音质扩展，但不降低原歌曲音质。默认保留原单曲，未收录的 B 面和特别录音始终保留。','hint'));box.addWidget(button('选择单曲，升级为完整专辑',self.choose_single_upgrade))
        box.addWidget(label('替换前核对曲目、发行和全部音频。旧版移出音乐库后保留短期恢复副本；下载与做种源不改动。','hint'));box.addStretch();box.addWidget(button('检查并自动升级音质',lambda:self.start('upgrade'),True))
    def choose_single_upgrade(self):
        from musicops.gui.catalog import LibraryCatalogDialog
        self.catalog_window=LibraryCatalogDialog(self,single_upgrade=True);self.catalog_window.open()
    def _tasks(self):
        box=self.page('任务中心','歌单任务依次识别、搜索、下载入库；本地资料补全独立显示，不下载音源。专辑总览显示当前步骤，阶段记录可追溯历史。')
        box.setSpacing(12);box.itemAt(0).widget().layout().setContentsMargins(20,14,20,14)
        self.checked_jobs={};self.job_items={};self.job_groups={};self.job_detail_cache={};self.job_detail_notes={};self.job_track_versions={};self.job_detail_requests=set();self.expanded_job_ids=set();self.bulk_pending=False
        self.execution_note=label('正在连接后台…','hint');box.addWidget(self.execution_note);self.task_summary=label('正在读取专辑队列…','hint');box.addWidget(self.task_summary)
        row=WrapLayout();self.task_view=QComboBox();self.task_view.addItem('专辑总览','albums');self.task_view.addItem('阶段记录','steps');self.task_view.currentIndexChanged.connect(self.change_task_view);row.addWidget(self.task_view);row.addWidget(label('状态'));self.task_filter=QComboBox()
        for name,key in (('进行中 / 等待','active'),('需要核对','review'),('失败','failed'),('已结束','completed'),('全部','all')):self.task_filter.addItem(name,key)
        self.task_filter.currentIndexChanged.connect(self.change_task_filter);row.addWidget(self.task_filter);row.addStretch()
        row.addWidget(button('重试全部失败',lambda:self.all_task_action('retry')));row.addWidget(button('AI 核对全部待核对任务',lambda:self.all_task_action('ai')));row.addWidget(button('逐项人工核对',lambda:self.all_task_action('manual')));box.addLayout(row)
        row=WrapLayout();self.job_select_page=QCheckBox('勾选本页');self.job_select_page.setTristate(True);self.job_select_page.stateChanged.connect(self.select_job_page);row.addWidget(self.job_select_page);self.job_selection_note=label('已勾选 0 项','hint');row.addWidget(self.job_selection_note);row.addStretch();self.task_query=line('','搜索全部专辑 / 艺人');self.task_query.setMaximumWidth(240);self.task_query.setMaxLength(200);self.task_query_timer=QTimer(self);self.task_query_timer.setSingleShot(True);self.task_query_timer.setInterval(350);self.task_query_timer.timeout.connect(self.change_task_filter);self.task_query.textChanged.connect(lambda _:self.task_query_timer.start());row.addWidget(self.task_query);row.addWidget(button('清空勾选',self.clear_checked_jobs));box.addLayout(row)
        self.selection_actions=QWidget();row=WrapLayout(self.selection_actions);row.setContentsMargins(0,0,0,0);self.bulk_buttons=[]
        for text,action in (('暂停选中任务','pause'),('恢复选中任务','resume'),('取消选中任务','cancel'),('重试选中失败任务','retry'),('删除选中记录','delete')):
            action_button=button(text,lambda checked=False,a=action:self.bulk_action(a));self.bulk_buttons.append(action_button);row.addWidget(action_button)
        self.bulk_ai_button=button('AI 核对选中任务',self.explain);row.addWidget(self.bulk_ai_button);row.addStretch();box.addWidget(self.selection_actions)
        self.job_table=QTreeWidget();self.job_table.setColumnCount(5);self.job_table.setHeaderLabels(['任务 / 专辑 / 歌曲','类型 / 艺人','当前步骤 / 专辑','进度','结果']);self.job_table.setAlternatingRowColors(True);self.job_table.setSelectionMode(QAbstractItemView.SingleSelection);self.job_table.setEditTriggers(QAbstractItemView.NoEditTriggers);self.job_table.setUniformRowHeights(True);self.job_table.setIndentation(20);self.job_table.setRootIsDecorated(True)
        header=self.job_table.header();header.setDefaultAlignment(Qt.AlignLeft|Qt.AlignVCenter);header.setSectionResizeMode(0,QHeaderView.Interactive);self.job_table.setColumnWidth(0,270);header.setSectionResizeMode(1,QHeaderView.Interactive);self.job_table.setColumnWidth(1,125)
        for column in (2,3):header.setSectionResizeMode(column,QHeaderView.ResizeToContents)
        header.setSectionResizeMode(4,QHeaderView.Stretch)
        self.job_table.setMinimumHeight(240);box.addWidget(self.job_table,1);self.job_detail=QPlainTextEdit();self.job_detail.setReadOnly(True);self.job_detail.setPlaceholderText('选择一项查看结果；勾选任务后可批量操作。');self.job_detail.setFixedHeight(80);box.addWidget(self.job_detail);self.job_table.itemSelectionChanged.connect(self.show_detail);self.job_table.itemChanged.connect(self.job_check_changed);self.job_table.itemExpanded.connect(self.expand_job);self.job_table.itemCollapsed.connect(self.collapse_job)
        actions=WrapLayout();self.review_current=button('核对当前专辑',self.review);self.retry_current=button('重试当前任务',lambda:self.bulk_action('retry',[self.selected_job()]) if self.selected_job() else None);self.restore_current=button('恢复当前任务原版',self.restore)
        for action in (self.review_current,self.retry_current,self.restore_current):action.setEnabled(False);actions.addWidget(action)
        actions.addStretch();actions.addWidget(button('暂停整个后台',lambda:self.call('pause_pipeline',{},lambda _:self.refresh())));actions.addWidget(button('继续整个后台',lambda:self.call('resume_pipeline',{},lambda _:self.refresh())));actions.addWidget(button('退出应用及后台',self.exit_background));box.addLayout(actions)
        pages=WrapLayout();self.job_counts=label('');self.job_counts.setWordWrap(False);pages.addWidget(self.job_counts);pages.addStretch();pages.addWidget(button('上一页',lambda:self.job_page(-200)));pages.addWidget(button('下一页',lambda:self.job_page(200)));box.addLayout(pages)
        self.update_job_selection()
    def _duplicates(self):
        from musicops.gui.duplicates import DuplicatePage
        box=self.page('去重与合并专辑','扫描重复歌曲和分散的专辑副本。可靠项目自动合并，冲突版本分别保留，旧副本支持恢复。')
        self.duplicate_page=DuplicatePage(self);box.addWidget(self.duplicate_page,1)
    def check_updates(self,manual=False):
        if os.environ.get('MUSICOPS_CONTAINER')=='1':
            if manual:self.update_note.setText('Docker 更新：在宿主机运行 docker compose pull && docker compose up -d；配置和任务保留在持久卷。')
            return
        if not self.updates_enabled:
            if manual:self.update_note.setText('源码或验收运行不启动自动安装；安装版可自动检查更新。')
            return
        if not manual and not self.config.get('updates',{}).get('enabled',True):return
        if not self.update_controller:
            from musicops.gui.updates import UpdateController
            self.update_controller=UpdateController(self)
        self.update_controller.start(automatic=not manual)
    def refresh_catalog(self):
        if not self.config:return
        self.call('index_library',{'root':self.config['paths']['library'],'lightweight':True},lambda result:(self.notice.setText('本地专辑资料库已更新：'+str(result.get('count',0))+' 个音频文件'),self.refresh()))
    def browse_catalog(self):
        from musicops.gui.catalog import LibraryCatalogDialog
        self.catalog_window=LibraryCatalogDialog(self);self.catalog_window.open()
    def setup(self,browser=False):
        if not self.config:return
        self.wizard=SetupWizard(self)
        if browser:self.wizard.setStartId(self.wizard.solver_page_id)
        self.wizard.open()
    def exit_background(self):
        self.timer.stop();self.notice.setText('正在安全退出后台…')
        def stopped(_):self.close()
        def failed(message):
            self.notice.setText('后台仍在处理任务；请等待完成，或在任务中心取消对应任务后再退出。' if '当前任务' in message else '退出未完成：'+message)
            self.timer.start();self.update_activity()
        self.call('shutdown_worker',{},stopped,failed)
    def _album_search(self):
        box=self.page('按歌手浏览，或寻找指定专辑','同时检索 OpenCD、DicMusic、Orpheus 和 RED。歌手模式按专辑、发行版本及站点音源逐层展开。')
        modes=QHBoxLayout();self.album_literal=QRadioButton('原样搜索输入内容');self.album_precise=QRadioButton('识别专辑后精准搜索');self.artist_mode=QRadioButton('按歌手检索全部专辑');self.album_literal.setChecked(True);modes.addWidget(self.album_literal);modes.addWidget(self.album_precise);modes.addWidget(self.artist_mode);modes.addStretch();box.addLayout(modes)
        row=QHBoxLayout();self.album_query=line('','输入专辑名，或 艺人 - 专辑名');row.addWidget(self.album_query,2);self.album_artist=line('','艺人（精准识别时可选）');self.album_artist.setEnabled(False);self.album_precise.toggled.connect(self.album_artist.setEnabled);row.addWidget(self.album_artist,1);row.addWidget(button('搜索四站',self.search_album,True));box.addLayout(row)
        self.album_query.setMaxLength(300)
        self.album_note=label('原样模式保留输入内容（最多 300 字符）。精准模式先列出专辑资料，由你确认对应发行，再按规范艺人和专辑名检索。','hint');box.addWidget(self.album_note)
        self.album_results=self.table(['站点','专辑 / 资源','艺人','音质','做种','匹配']);self.album_results.horizontalHeader().setSectionResizeMode(0,QHeaderView.ResizeToContents);self.album_results.horizontalHeader().setSectionResizeMode(4,QHeaderView.ResizeToContents);box.addWidget(self.album_results)
        self.artist_tree=QTreeWidget();self.artist_tree.setColumnCount(5);self.artist_tree.setHeaderLabels(['歌手 / 专辑 / 版本 / 音源','年份 / 音质','站点','做种','分组依据']);self.artist_tree.setUniformRowHeights(True);self.artist_tree.header().setSectionResizeMode(0,QHeaderView.Stretch);self.artist_tree.setColumnWidth(1,190);self.artist_tree.setVisible(False);box.addWidget(self.artist_tree,1)
        controls=WrapLayout();self.artist_stop=button('停止继续读取',lambda:setattr(self,'artist_autoload',False));self.artist_ai=button('AI 辅助识别分组',lambda:self.artist_more(force_ai=True));self.artist_fix=button('修正所选音源的专辑分组',self.fix_artist_group);self.artist_retry=button('重试未完成的站点',lambda:self.artist_more(retry_sites=True))
        self.artist_continue=button('继续读取剩余页',self.continue_artist)
        self.artist_controls=(self.artist_stop,self.artist_continue,self.artist_ai,self.artist_fix,self.artist_retry)
        for control in self.artist_controls:controls.addWidget(control);control.setVisible(False)
        box.addLayout(controls);self.artist_result=None;self.artist_autoload=False;self.artist_generation=0;self.artist_busy=False
        row=QHBoxLayout();row.addWidget(label('歌手模式按站点可用分页继续读取；结果按专辑和版本展开，分组线索不等于录音身份已确认。','hint'));row.addStretch();row.addWidget(button('打开选中资源详情',self.open_album_result));box.addLayout(row);self.manual_candidates={}
    def search_album(self):
        query=self.album_query.text()
        self.artist_generation+=1
        artist_mode=self.artist_mode.isChecked();self.artist_tree.setVisible(artist_mode);self.album_results.setVisible(not artist_mode)
        for control in self.artist_controls:control.setVisible(artist_mode)
        self.artist_busy=False;self.artist_autoload=False
        if artist_mode:
            self.artist_result=None;self.artist_autoload=True;self.artist_fetch({'mode':'artist','query':query});return
        if not query.strip():self.album_note.setText('请先输入要检索的专辑或内容。');return
        self.album_results.setRowCount(0);self.manual_candidates={};self.album_note.setText('检索已提交；后台有其他任务时会排队，请稍候…')
        payload={'mode':'precise' if self.album_precise.isChecked() else 'literal','query':query,'artist':self.album_artist.text()}
        generation=self.artist_generation
        def received(result):
            if generation!=self.artist_generation:return
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
    def artist_fetch(self,payload):
        if self.artist_busy:return
        self.artist_busy=True
        for control in (self.artist_continue,self.artist_ai,self.artist_fix,self.artist_retry):control.setEnabled(False)
        generation=self.artist_generation
        self.album_note.setText('正在按歌手读取站点专辑及发行版本…')
        def finished():
            self.artist_busy=False
            for control in (self.artist_continue,self.artist_ai,self.artist_fix,self.artist_retry):control.setEnabled(True)
        def failed(error):
            if generation!=self.artist_generation:return
            finished();self.album_note.setText('歌手检索未完成：'+str(error))
        def received(result):
            if generation!=self.artist_generation:return
            finished()
            if result.get('error'):self.album_note.setText('歌手检索未完成：'+str(result['error']));return
            self.artist_result=result;self.show_artist_results(result)
            if result.get('has_more') and self.artist_autoload:QTimer.singleShot(300,lambda:self.artist_more() if self.artist_autoload and generation==self.artist_generation else None)
        self.call('manual_album_search',payload,received,failed)
    def continue_artist(self):
        if not self.artist_result or not self.artist_result.get('has_more'):return
        self.artist_autoload=True;self.artist_more()
    def artist_more(self,**options):
        if not self.artist_result or self.artist_busy:return
        self.artist_fetch({'mode':'artist','query':self.artist_result['query'],'artist_result_id':self.artist_result['artist_result_id'],**options})
    def show_artist_results(self,result):
        expanded=set()
        iterator=QTreeWidgetItemIterator(self.artist_tree)
        while iterator.value():
            item=iterator.value()
            if item.isExpanded():expanded.add(item.data(0,Qt.UserRole+1))
            iterator+=1
        scroll=self.artist_tree.verticalScrollBar().value();self.artist_tree.setUpdatesEnabled(False);self.artist_tree.clear();self.manual_candidates={}
        root=QTreeWidgetItem(self.artist_tree,[result['query']]);root.setData(0,Qt.UserRole+1,'artist');root.setExpanded(True)
        for album in result.get('albums',[]):
            parent=QTreeWidgetItem(root,[album['title'],str(album.get('year') or ''),'','','艺人归属 / 专辑待核对' if album.get('uncertain') else '跨站分组线索']);parent.setData(0,Qt.UserRole+1,album['group_id']);parent.setExpanded(album['group_id'] in expanded)
            for version in album['versions']:
                item=QTreeWidgetItem(parent,[version['label'],str(version.get('year') or ''),'','','目录号 '+str(version.get('catalogue') or '未知')]);item.setData(0,Qt.UserRole+1,version['group_id']);item.setExpanded(version['group_id'] in expanded)
                for candidate in version['candidates']:
                    media=candidate.get('media') or {};quality=' '.join(str(media.get(k) or '') for k in ('format','encoding','media'));source=dict((key,name) for key,name,_ in SITES).get(candidate['source'],candidate['source'])
                    leaf=QTreeWidgetItem(item,[candidate['title'],quality,source,str(candidate.get('seeders') or 0),'手动核对' if candidate.get('manual_grouping') else 'AI 分组线索' if candidate.get('ai_grouping') else '站点分组 '+str(candidate.get('group_id') or ' / 标题解析')]);leaf.setData(0,Qt.UserRole,candidate['candidate_id']);leaf.setToolTip(0,candidate['title']);self.manual_candidates[candidate['candidate_id']]=candidate
        self.artist_tree.setUpdatesEnabled(True);self.artist_tree.verticalScrollBar().setValue(scroll);self.album_note.setText(result.get('message','')+(' 正在继续读取剩余页…' if result.get('has_more') and self.artist_autoload else '')+'\n'+'；'.join(value.get('message',key) for key,value in result.get('source_outcomes',{}).items()))
    def fix_artist_group(self):
        item=self.artist_tree.currentItem();candidate=self.manual_candidates.get(item.data(0,Qt.UserRole)) if item else None
        if not candidate:self.album_note.setText('请展开专辑和版本，选择一条具体音源再修正分组。');return
        self.artist_autoload=False
        title,ok=QInputDialog.getText(self,'核对专辑分组','确认属于当前检索歌手后填写专辑名；现场、混音、重录等版本仍分别显示。',text=candidate.get('manual_album_label') or candidate.get('album_label') or candidate['title'])
        if ok:self.artist_more(group_override={'candidate_id':candidate['candidate_id'],'album':title})
    def open_album_result(self):
        if self.artist_mode.isChecked():
            item=self.artist_tree.currentItem();candidate=self.manual_candidates.get(item.data(0,Qt.UserRole)) if item else None
            if candidate:self.open_candidate_page(candidate)
            return
        row=self.album_results.currentRow()
        if row<0:return
        candidate=self.manual_candidates.get(self.album_results.item(row,0).data(Qt.UserRole))
        if not candidate:return
        self.open_candidate_page(candidate)
    def open_candidate_page(self,candidate):
        rid=str(candidate.get('resource_id',''));site=candidate['source'];gid=str(candidate.get('group_id',''))
        if not rid.isdigit():return
        host=next((host for key,_,host in SITES if key==site),None)
        if not host:return
        path='/details.php?id='+rid if site=='open.cd' else '/torrents.php?id='+(gid if gid.isdigit() else rid)+'&torrentid='+rid
        QDesktopServices.openUrl(QUrl('https://'+host+path))
    def call(self,command,payload,done=None,failed=None,accepted=None):
        if not hasattr(self,'inflight_reads'):self.inflight_reads=set();self.pool.setMaxThreadCount(8)
        read_key=(command,json.dumps(payload,sort_keys=True,ensure_ascii=False)) if command in ('get_status','list_workflows') else None
        if read_key in self.inflight_reads:return
        if read_key:self.inflight_reads.add(read_key)
        request=Request(self.client,command,payload);self.requests.add(request);self.update_activity()
        def success(result):
            if read_key:self.inflight_reads.discard(read_key)
            self.requests.discard(request)
            if result.get('accepted') and done:
                self.tasks[result['task_id']]=done;self.task_failures[result['task_id']]=failed;self.timer.start();QTimer.singleShot(0,self.poll_tasks)
                if accepted:accepted(result)
            elif done:done(result)
            self.update_activity()
        def failure(message):
            if read_key:self.inflight_reads.discard(read_key)
            self.requests.discard(request)
            lost=message.startswith(('[Errno ','[WinError ')) or 'IPC_CONNECTION_ERROR' in message or '后台响应超时' in message
            if failed:failed(message)
            elif lost:
                background=command in ('attach','get_status','list_workflows','get_task','list_subscriptions')
                interrupted='后台连接中断，正在重新连接；连接恢复后请重试刚才的操作。'
                # Keep telling the user an action did not run until the worker is back.
                if not (background and self.notice.text()==interrupted):self.notice.setText('正在重新连接后台，请稍候…' if background else interrupted)
                self._connection_notice=self.notice.text();self.timer.start()
            else:self.notice.setText('需要处理：'+message)
            # A stopped or crashed worker leaves no pipe; restart it once instead
            # of polling a missing pipe forever.
            if lost and command!='attach' and not getattr(self,'_reattaching',False):
                self._reattaching=True
                def reattached(_):self._reattaching=False;self.refresh()
                self.call('attach',{},reattached,failed=lambda _:setattr(self,'_reattaching',False))
            self.update_activity()
        if command=='attach':request.signals.progress.connect(self.connection.setText)
        request.signals.done.connect(success);request.signals.failed.connect(failure);self.pool.start(request,0 if command in ('get_status','list_workflows','get_task') else 1)
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
                if task.get('state') not in ('completed','failed','blocked','cancelled','recovery_required'):
                    if task.get('command')=='refresh_playlist' and task.get('step'):self.playlist_note.setText(str(task['step']))
                    return
                callback=self.tasks.pop(task_id,None)
                raw_result=task.get('result')
                result=raw_result if isinstance(raw_result,dict) and raw_result else {'error':task.get('error','任务未完成')}
                has_playlist_targets=task.get('command')=='refresh_playlist' and isinstance(raw_result,dict) and bool(raw_result.get('targets'))
                if task.get('state')!='completed' and not has_playlist_targets:self.notice.setText('任务未完成：'+str(task.get('error') or result.get('reason') or task.get('state')))
                error_callback=self.task_failures.pop(task_id,None)
                if task.get('state')!='completed' and not has_playlist_targets and error_callback:error_callback(str(task.get('error') or result.get('reason') or task.get('state')))
                elif callback:callback(result)
                self.update_activity()
            def failed(message,task_id=tid):
                self.task_polls.discard(task_id);self.notice.setText('正在重新连接任务：'+message);self.update_activity()
            self.call('get_task',{'task_id':tid},received,failed)
    def refresh(self):
        self.poll_tasks()
        if len(self.requests)>5:return
        self.call('get_status',{'compact':True},self.status)
        requested_filter=self.task_filter.currentData()
        requested_offset=self.job_offset
        requested_view=self.task_view.currentData()
        requested_query=self.task_query.text().strip()
        self.call('list_workflows',{'offset':requested_offset,'state':requested_filter,'view':requested_view,'query':requested_query},lambda result:self.show_jobs(result) if self.task_filter.currentData()==requested_filter and self.job_offset==requested_offset and self.task_view.currentData()==requested_view and self.task_query.text().strip()==requested_query else None)
    def status(self,result):
        if self.notice.text()==getattr(self,'_connection_notice',None):self.notice.clear()
        self._connection_notice=None
        self.pipeline_paused=bool(result.get('paused'));self.config=result['config']['values'];self.available_refs=[r['ref'] if isinstance(r,dict) else r for r in result.get('secret_refs',[])];self.timer.start();connected=[name for key,name,_ in SITES if self.config['providers'][key]['enabled'] and self.config['providers'][key]['credential_ref'] in self.available_refs]
        executing=result.get('execution') or {};names={'restore_duplicates':'恢复合并副本','scan_duplicates':'校验重复歌曲与专辑','merge_duplicates':'合并专辑副本','normalize_qbt_tags':'整理 MusicOps 下载标签','index_library':'扫描本地音乐库','advance_workflows':'推进专辑处理','assist_workflows':'AI 辅助核对','refresh_playlist':'读取歌单','start_workflow':'创建专辑队列','browser_solver_preflight':'验证浏览器连接'}
        self.execution_note.setText('后台已暂停，请继续后台后处理队列。' if self.pipeline_paused else '后台正在：'+names.get(executing.get('command'),'处理当前任务') if executing else '后台等待下一项可执行任务；失败和待核对专辑可按状态查看。')
        performance=result.get('performance',{});self.performance=performance
        scheduling='小核后台' if performance.get('mode')=='efficiency' else '低优先级后台' if performance.get('low_priority') else '系统调度'
        self.execution_note.setToolTip(scheduling+'；文件校验最多 4 项并行，界面请求最多 8 项；音频解码每项 1 线程。')

        catalog=result.get('local_catalog') or {};self.catalog_note.setText('本地专辑资料库：'+str(catalog.get('album_count',0))+' 张专辑 · '+str(catalog.get('matched_track_count',0))+' 首已预匹配'+(' · 扫描时增量更新' if catalog.get('updated_at') else ' · 首次扫描后自动建立'))
        self.connection.setText('音乐库：'+self.config['paths']['library']+'     ·     '+('已配置 '+', '.join(connected) if connected else '尚未连接资源站；可在“连接与设置”添加'))
        if not self.wizard_shown and not self.config['workflow']['setup_done']:
            self.wizard_shown=True;QTimer.singleShot(200,self.setup)
        self.poll_tasks()
        if not self.update_checked and self.updates_enabled:
            self.update_checked=True;QTimer.singleShot(1500,self.check_updates)
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
            if result.get('reason') and not result.get('targets'):self.playlist_note.setText(self.playlist_note.text()+' '+result['reason'])
        self.call('refresh_playlist',payload,parsed,lambda error:self.playlist_note.setText('读取未完成：'+error))
    def load_subscriptions(self):
        def loaded(result):
            if result.get('error'):return
            import datetime as dt
            def when(value):
                if not value:return '—'
                try:moment=dt.datetime.fromtimestamp(value) if isinstance(value,(int,float)) else dt.datetime.fromisoformat(str(value)).astimezone()
                except (ValueError,OSError):return '—'
                return moment.strftime('%m-%d %H:%M')
            self.subscriptions=result.get('subscriptions',[]);table=self.subscription_table;table.setRowCount(len(self.subscriptions))
            for i,row in enumerate(self.subscriptions):
                values=[(row.get('name') or row['playlist_input'])+('' if row.get('enabled') else '（已暂停）'),f"{row['interval_hours']} 小时",when(row.get('last_checked_at')),row.get('last_reason') or '等待首次检查',when(row.get('next_check_at')) if row.get('enabled') else '—']
                for j,value in enumerate(values):
                    item=QTableWidgetItem(value);item.setToolTip(row['playlist_input'] if j==0 else value);item.setData(Qt.UserRole,row['subscription_id']);table.setItem(i,j,item)
        self.call('list_subscriptions',{},loaded)
    def _selected_subscription(self):
        row=self.subscription_table.currentRow()
        if row<0 or not self.subscription_table.item(row,0):self.subscription_note.setText('请先在列表中选择一个订阅。');return None
        return self.subscription_table.item(row,0).data(Qt.UserRole)
    def _subscription_done(self,message):
        def done(result):
            self.subscription_note.setText(result['error'] if result.get('error') else message);self.load_subscriptions()
        return done
    def add_subscription(self):
        text=self.playlist.text().strip()
        if not text:self.subscription_note.setText('请先在上方粘贴要订阅的歌单链接。');return
        self.call('save_subscription',{'playlist_input':text,'platform':self.playlist_platform.currentData(),'import_existing':self.subscription_existing.isChecked(),
                                       'interval_hours':self.subscription_hours.value(),'prefer_full_album':self.playlist_full_album.isChecked()},self._subscription_done('已订阅，马上进行首次检查。'))
    def check_subscription(self):
        sid=self._selected_subscription()
        if sid:self.call('check_subscription_now',{'subscription_id':sid},self._subscription_done('检查完成。'),lambda error:self.subscription_note.setText('检查未完成：'+error),lambda _:self.subscription_note.setText('正在检查歌单…'))
    def toggle_subscription(self):
        sid=self._selected_subscription();row=next((r for r in getattr(self,'subscriptions',[]) if r['subscription_id']==sid),None)
        if row:self.call('save_subscription',{'subscription_id':sid,'enabled':not row.get('enabled')},self._subscription_done('已恢复订阅。' if not row.get('enabled') else '已暂停订阅。'))
    def delete_subscription(self):
        sid=self._selected_subscription()
        if sid and QMessageBox.question(self,'删除订阅','删除后不再检查这个歌单；已入库的音乐不受影响。')==QMessageBox.Yes:
            self.call('delete_subscription',{'subscription_id':sid},self._subscription_done('已删除订阅。'))
    def load_cached_playlist(self):
        def loaded(result):
            if not result.get('targets'):
                self.playlist_note.setText('本机还没有歌单缓存，请先解析歌单或导入文件。');return
            self.loaded_tracks(result)
        self.call('get_playlist_targets',{},loaded,lambda error:self.playlist_note.setText('缓存读取未完成：'+error))
    def loaded_tracks(self,result):
        self.targets=result['targets'];self.track_table.setRowCount(len(self.targets))
        self.track_table.setUpdatesEnabled(False)
        for i,target in enumerate(self.targets):
            check=QTableWidgetItem();check.setFlags(Qt.ItemIsUserCheckable|Qt.ItemIsEnabled);check.setCheckState(Qt.Unchecked);check.setData(Qt.UserRole,target['target_id']);self.track_table.setItem(i,0,check)
            state='待检查音乐库' if target.get('metadata_complete',True) else '资料缺失，需识别'
            if target.get('source'):state=PLATFORMS.get(target['source'],target['source'])+' · '+state
            for j,value in enumerate((target['raw_title'],' / '.join(target['artists']),target.get('album') or '待识别',state),1):self.track_table.setItem(i,j,QTableWidgetItem(value))
        self.track_table.setUpdatesEnabled(True)
        incomplete_count=sum(not t.get('metadata_complete',True) for t in self.targets)
        suffix=(f'（含 {incomplete_count} 首待识别专辑资料）' if incomplete_count else '')+(' 来源清单未完全对账，仅处理你明确选择的可见曲目。' if not result.get('complete',True) else '')
        self.playlist_note.setText(('已载入本机缓存，尚未重新联网核实。' if result.get('from_cache') else '')+f'已载入 {len(self.targets)} 首；选择需要入库的曲目。任务会按专辑去重。'+suffix)
    def select_all(self):
        checked=not all(self.track_table.item(i,0).checkState()==Qt.Checked for i in range(self.track_table.rowCount()))
        self.track_table.setUpdatesEnabled(False)
        for i in range(self.track_table.rowCount()):self.track_table.item(i,0).setCheckState(Qt.Checked if checked else Qt.Unchecked)
        self.track_table.setUpdatesEnabled(True)
    def start(self,scene,refresh_resources=False,**options):
        if not self.config.get('workflow',{}).get('setup_done'):self.setup();return
        targets=[self.track_table.item(i,0).data(Qt.UserRole) for i in range(self.track_table.rowCount()) if self.track_table.item(i,0).checkState()==Qt.Checked] if scene=='playlist' else []
        if scene=='playlist' and not targets:self.playlist_note.setText('请先选择要入库的曲目。');return
        self.notice.setText('任务已提交，正在准备音乐库检查…');self.notice.setVisible(True);self.job_offset=0;self.task_view.setCurrentIndex(self.task_view.findData('albums'));self.task_filter.setCurrentIndex(self.task_filter.findData('active'));self.nav.setCurrentRow(3)
        def started(result):
            if result.get('error'):self.notice.setText('任务未创建：'+result['error'])
            else:self.notice.setText(result.get('message','任务已创建'))
            self.refresh()
        self.call('start_workflow',{'scene':scene,'target_ids':targets,'prefer_24bit':self.prefer24.isChecked(),'refresh_resources':refresh_resources,'prefer_full_album':scene=='playlist' and self.playlist_full_album.isChecked(),**options},started)
    def show_jobs(self,result):
        scene_counts=result.get('scene_active_counts',{})
        scene_note='当前仅有本地资料补全，不会搜索或下载音源。 ' if scene_counts.get('complete') and not any(scene_counts.get(k) for k in ('playlist','upgrade','fill','expand')) else ''
        scene_note+=f"歌单入库 {scene_counts.get('playlist',0)} 项 · 本地资料补全 {scene_counts.get('complete',0)} 项。 " if scene_counts else ''
        counts=result.get('stage_counts',{});self.task_summary.setText(('后台已暂停 · 点击“继续整个后台”推进队列。 ' if getattr(self,'pipeline_paused',False) else scene_note+'后台按专辑推进，下载与入库优先。 ')+f"识别 / 搜索 {sum(counts.get(k,0) for k in ('identifying','snapshotting','searching'))} · 等待下载 {counts.get('requesting',0)+counts.get('fallback',0)} · 下载中 {counts.get('downloading',0)} · 整理 / 入库 {sum(counts.get(k,0) for k in ('metadata','enrich','validating','notifying'))} · 待核对 {counts.get('needs_review',0)} · 失败 {counts.get('failed',0)}" if result.get('view')=='albums' else '阶段记录：识别 / 搜索完成表示该步骤结束；请切换“专辑总览”查看后续下载与入库。')
        self.job_total=result.get('total',0);self.active_jobs=result.get('active',0);self.paused_jobs=result.get('paused_count',0);self.job_counts.setText(f"共 {self.job_total} 项 · 待处理 {self.active_jobs} · 暂停 {self.paused_jobs} · 完成 {result.get('finished',0)} · 第 {self.job_offset//200+1} 页")
        selected=self.selected_job();jobs=result.get('jobs',[]);collapsed={key for key,item in self.job_groups.items() if not item.isExpanded()};scroll=self.job_table.verticalScrollBar().value();anchor=self.scroll_anchor()
        groups=tuple(reversed(TASK_GROUPS)) if result.get('view')=='albums' else TASK_GROUPS
        reuse=list(self.job_items)==[j['job_id'] for j in jobs] and list(self.job_groups)==[key for key,_ in groups] and all(self.job_items[j['job_id']].parent() is self.job_groups.get(self.job_group(j)) for j in jobs)
        self.jobs={j['job_id']:j for j in jobs};self.job_table.blockSignals(True);self.job_table.setUpdatesEnabled(False)
        if not reuse:self.job_table.clear();self.job_items={};self.job_groups={}
        for key,title in ([] if reuse else groups):
            group=QTreeWidgetItem(self.job_table,[title]);group.setData(0,Qt.UserRole+1,key);group.setFlags(Qt.ItemIsEnabled|Qt.ItemIsUserCheckable);group.setCheckState(0,Qt.Unchecked);group.setExpanded(key not in collapsed);group.setForeground(0,QColor('#8ce6d2'));font=group.font(0);font.setBold(True);group.setFont(0,font);self.job_groups[key]=group
        for job in jobs:
            if job.get('overview'):
                previous=[r['job_id'] for r in job.get('chain',[]) if r['job_id'] in self.checked_jobs and r['job_id']!=job['job_id']]
                if previous:
                    for identity in previous:self.checked_jobs.pop(identity,None)
                    self.checked_jobs[job['job_id']]=self.ai_job_context(job)
            job_id=job['job_id'];batch=job.get('job_kind')=='batch';group_key=self.job_group(job);stage=job.get('stage','queued');progress='扫描中' if batch and stage=='preparing' and not job.get('progress') else str(job.get('progress',0))+'%'
            paused=bool(job.get('paused')) and stage not in ('completed','failed','cancelled','skipped','restored','needs_review')
            step='阶段结束后暂停' if paused and job.get('pause_pending') else '已暂停' if paused else STAGES.get(stage,stage)
            kind='检查本地库 · 批次' if batch else {'identification':'专辑识别','review_assistance':'AI 辅助核对','library_index':'本地专辑索引','catalog_identification':'在线资料识别','manual_search':'手动专辑检索','search':'专辑音源检索'}.get(job.get('task_type'),SCENES.get(job.get('scene'),job.get('scene','专辑任务')))
            values=[job.get('label','音乐库'),kind,step,progress,job.get('message','')+(' · '+job['ai_review_message'] if job.get('ai_review_message') else '')+(' · AI 建议待核对' if (job.get('ai_assistance') or {}).get('status')=='READY' else '')]
            item=self.job_items[job_id] if reuse else QTreeWidgetItem(self.job_groups[group_key],values)
            for column,value in enumerate(values):item.setText(column,value)
            item.setData(0,Qt.UserRole,job_id);item.setData(0,Qt.UserRole+1,'job');item.setFlags(Qt.ItemIsEnabled|Qt.ItemIsSelectable|Qt.ItemIsUserCheckable);item.setCheckState(0,Qt.Checked if job_id in self.checked_jobs else Qt.Unchecked);self.job_items[job_id]=item
            for column in range(5):item.setToolTip(column,item.text(column)+('\n'+job.get('detail','') if column==4 and job.get('detail') else ''))
            item.setForeground(2,QColor('#f4c77c' if paused or stage in ('failed','needs_review') else '#6ee7d0' if stage in ('completed','restored') else '#9bbbf4'))
            if job_id in self.checked_jobs:self.checked_jobs[job_id]=self.ai_job_context(job)
            if not batch:
                track_version=(stage,job.get('target_count'),job.get('matched_count'))
                if self.job_track_versions.get(job_id)!=track_version:self.job_detail_cache.pop(job_id,None);self.job_detail_notes.pop(job_id,None)
                self.job_track_versions[job_id]=track_version
                if job_id in self.job_detail_cache and job_id in self.expanded_job_ids:self.fill_job_tracks(item,self.job_detail_cache[job_id])
                elif not reuse:QTreeWidgetItem(item,['展开查看歌曲明细'])
                item.setExpanded(job_id in self.expanded_job_ids)
            if job_id==selected and not reuse:self.job_table.setCurrentItem(item)
        for key,item in self.job_groups.items():item.setText(1,f'{item.childCount()} 项（本页）')
        if not selected and jobs:
            preferred=next((job for job in jobs if job.get('job_kind')=='batch' and job.get('stage')=='preparing'),jobs[0]);self.job_table.setCurrentItem(self.job_items[preferred['job_id']])
        self.job_table.blockSignals(False);self.job_table.setUpdatesEnabled(True);self.job_table.verticalScrollBar().setValue(scroll);self.update_job_selection();self.show_detail();self.update_activity()
        if not reuse:self.restore_scroll(anchor,scroll)
        if not getattr(self,'token_prompt_open',False):
            seen=getattr(self,'token_prompt_seen',set());self.token_prompt_seen=seen
            pending=next((job_id for job_id in result.get('token_prompts',[]) if job_id not in seen),None)
            if pending:seen.add(pending);self.token_prompt_open=True;QTimer.singleShot(0,lambda:self.token_prompt(pending))
        for job_id in self.expanded_job_ids.intersection(self.job_items):
            if job_id not in self.job_detail_cache:self.expand_job(self.job_items[job_id])
    @staticmethod
    def job_group(job):
        if job.get('scene')=='complete' and job.get('job_kind') not in ('batch','activity'):return 'repair'
        if job.get('task_group') in dict(TASK_GROUPS):return job['task_group']
        if job.get('job_kind')=='batch' or job.get('task_type')=='identification' or job.get('stage') in ('identifying','preparing','planning','enrich','metadata'):return 'identify'
        if job.get('stage') in ('searching','requesting') or job.get('identification_job_id'):return 'search'
        return 'process'
    @staticmethod
    def ai_job_context(job):
        # Details contain local directories and transport errors. Explain only
        # compact public state, removing embedded paths/links from display text.
        def clean(value):
            value=str(redact(str(value or '')))
            value=re.sub(r'https?://[^\s<>"\']+','[链接]',value,flags=re.I)
            value=re.sub(r'(?:[A-Za-z]:[\\/]|\\\\)[^\r\n;；|]*','[本地路径]',value)
            value=re.sub(r'(?<![\w:/])/(?:[^\s;；|]+/?)+','[本地路径]',value)
            value=re.sub(r'\b(?:api[_ -]?key|token|authorization|cookie|passkey|authkey|password)\s*[:=]\s*(?:Bearer\s+)?[^\s;；]+','[凭据]',value,flags=re.I)
            return value[:500]
        return {key:clean(job.get(key)) for key in ('job_id','scene','stage','label','message')}
    def job_check_changed(self,item,column):
        if column!=0:return
        role=item.data(0,Qt.UserRole+1);checked=item.checkState(0)==Qt.Checked
        if role=='job':items=[item]
        elif role in self.job_groups:items=[item.child(i) for i in range(item.childCount())]
        else:return
        self.job_table.blockSignals(True)
        for child in items:
            job_id=child.data(0,Qt.UserRole)
            if checked and job_id not in self.checked_jobs and len(self.checked_jobs)>=500:
                child.setCheckState(0,Qt.Unchecked);self.notice.setText('每次最多勾选 500 项；请先处理本批勾选，再继续其他任务。');continue
            child.setCheckState(0,Qt.Checked if checked else Qt.Unchecked)
            if checked:self.checked_jobs[job_id]=self.ai_job_context(self.jobs.get(job_id,{}))
            else:self.checked_jobs.pop(job_id,None)
        self.job_table.blockSignals(False);self.update_job_selection();self.update_activity()
    def update_job_selection(self):
        self.job_table.blockSignals(True)
        for group in self.job_groups.values():
            count=sum(group.child(i).data(0,Qt.UserRole) in self.checked_jobs for i in range(group.childCount()));group.setCheckState(0,Qt.Checked if count and count==group.childCount() else Qt.PartiallyChecked if count else Qt.Unchecked)
        self.job_table.blockSignals(False)
        count=sum(job_id in self.checked_jobs for job_id in self.job_items);self.job_select_page.blockSignals(True);self.job_select_page.setCheckState(Qt.Checked if count and count==len(self.job_items) else Qt.PartiallyChecked if count else Qt.Unchecked);self.job_select_page.blockSignals(False)
        off_page=len(self.checked_jobs)-count;self.job_selection_note.setText(f'已勾选 {len(self.checked_jobs)} 项'+(f' · 其他页 {off_page} 项' if off_page else ''))
        for action_button in self.bulk_buttons:action_button.setEnabled(bool(self.checked_jobs) and not self.bulk_pending)
        self.bulk_ai_button.setEnabled(bool(self.checked_jobs) and not getattr(self,'bulk_ai_pending',False))
        self.selection_actions.setVisible(bool(self.checked_jobs))
    def select_job_page(self,state):
        # A click on a partially selected page means select the entire page.
        checked=state!=Qt.Unchecked.value
        self.job_table.blockSignals(True)
        for job_id,item in self.job_items.items():
            if checked and job_id not in self.checked_jobs and len(self.checked_jobs)>=500:continue
            item.setCheckState(0,Qt.Checked if checked else Qt.Unchecked)
            if checked:self.checked_jobs[job_id]=self.ai_job_context(self.jobs[job_id])
            else:self.checked_jobs.pop(job_id,None)
        self.job_table.blockSignals(False);self.update_job_selection()
    def clear_checked_jobs(self):
        self.checked_jobs.clear();self.job_table.blockSignals(True)
        for item in self.job_items.values():item.setCheckState(0,Qt.Unchecked)
        self.job_table.blockSignals(False);self.update_job_selection()
    def expand_job(self,item):
        if item.data(0,Qt.UserRole+1)!='job':return
        job_id=item.data(0,Qt.UserRole);self.expanded_job_ids.add(job_id)
        if job_id in self.job_detail_cache:self.fill_job_tracks(item,self.job_detail_cache[job_id]);return
        if job_id in self.job_detail_requests:return
        self.job_detail_requests.add(job_id)
        def received(result):
            self.job_detail_requests.discard(job_id);job=result.get('job') or {};rows=self.job_child_rows(job)
            self.job_detail_cache[job_id]=rows or [['暂时没有歌曲明细']]
            while len(self.job_detail_cache)>50:
                oldest=next(iter(self.job_detail_cache));self.job_detail_cache.pop(oldest);self.job_detail_notes.pop(oldest,None)
            current=self.job_items.get(job_id)
            if current:self.fill_job_tracks(current,self.job_detail_cache[job_id])
            text=job.get('message','')+'\n'+job.get('detail','');names={key:name for key,name,_ in SITES};outcomes=job.get('source_outcomes') or job.get('outcomes') or {}
            if isinstance(outcomes,dict):
                states={'OK':'检索有结果','NO_RESOURCE':'检索完成，未找到资源','DISABLED':'未启用','AUTH_FAILED':'登录失效','AUTH_REQUIRED':'需要登录','RATE_LIMITED':'访问限流','NETWORK_ERROR':'网络连接失败','TRANSIENT_ERROR':'站点暂时不可用','PARSE_ERROR':'页面内容未识别','ACCESS_BLOCKED':'访问受限','SNAPSHOT_INCOMPLETE':'已读取的结果有限'}
                for source,outcome in outcomes.items():
                    if isinstance(outcome,dict):text+='\n'+names.get(source,source)+'：'+states.get(outcome.get('status'),outcome.get('status','未知'))+(' · '+str(outcome.get('reason')) if outcome.get('reason') else '')
            self.job_detail_notes[job_id]=text
            if self.selected_job()==job_id and not getattr(self,'detail_pinned',False):self.job_detail.setPlainText(text)
        def failed(message):
            self.job_detail_requests.discard(job_id);self.job_detail.setPlainText('读取歌曲明细失败：'+message+'\n收起后重新展开可重试。')
        self.call('get_workflow',{'job_id':job_id},received,failed)
    @staticmethod
    def job_child_rows(job):
        tracks=job.get('prepared') or job.get('targets') or job.get('old_files') or [];rows=[]
        def artists(row):
            credits=row.get('artists') or []
            return ' / '.join(str(value) for value in (credits if isinstance(credits,list) else [credits]))
        for track in tracks[:200]:rows.append([str(track.get('raw_title') or track.get('title') or '待识别歌曲'),artists(track),str(track.get('album') or '待识别专辑'),' / '.join(str(track.get(key) or '') for key in ('disc','track')).strip(' /'),PLATFORMS.get(track.get('source'),track.get('source',''))])
        if len(tracks)>200:rows.append([f'另有 {len(tracks)-200} 首；此处显示前 200 首'])
        candidates=job.get('choices') or job.get('catalog_choices') or []
        if candidates:
            search=job.get('task_group')=='search';names={key:name for key,name,_ in SITES};names.update(musicbrainz='MusicBrainz',discogs='Discogs',allmusic='AllMusic',rateyourmusic='Rate Your Music',albumoftheyear='Album of the Year',vgmdb='VGMdb')
            if rows:rows.append([f'{"检索结果" if search else "识别候选"} · {len(candidates)} 项'])
            for candidate in candidates[:200]:
                source=names.get(candidate.get('source'),candidate.get('source',''));title=str(candidate.get('album') or candidate.get('title') or '待核对专辑');media=candidate.get('media') or {};quality=' '.join(str(media.get(key) or '') for key in ('format','encoding','media')).strip();date=str(candidate.get('date') or candidate.get('year') or '')
                rows.append([title,artists(candidate),source,quality or date,('精准匹配' if candidate.get('precise_match') else '待核对发行')+(' · 做种 '+str(candidate['seeders']) if candidate.get('seeders') is not None else '')])
            if len(candidates)>200:rows.append([f'另有 {len(candidates)-200} 项结果；此处显示前 200 项'])
        return rows
    def collapse_job(self,item):
        if item.data(0,Qt.UserRole+1)=='job':self.expanded_job_ids.discard(item.data(0,Qt.UserRole))
    def fill_job_tracks(self,item,rows):
        if item.data(0,Qt.UserRole+2)==rows:return
        item.setData(0,Qt.UserRole+2,copy.deepcopy(rows))
        anchor=self.scroll_anchor();scroll=self.job_table.verticalScrollBar().value()
        item.takeChildren()
        for values in rows:
            track=QTreeWidgetItem(item,values);track.setData(0,Qt.UserRole+1,'track');track.setFlags(Qt.ItemIsEnabled|Qt.ItemIsSelectable)
            for column in range(len(values)):track.setToolTip(column,str(values[column]))
        self.restore_scroll(anchor,scroll)
    def scroll_anchor(self):
        item=self.job_table.itemAt(8,8)
        if item is None:return None
        child_index=None
        if item.data(0,Qt.UserRole+1)=='track':child_index=item.parent().indexOfChild(item);item=item.parent()
        return (item.data(0,Qt.UserRole),item.data(0,Qt.UserRole+1),child_index)
    def restore_scroll(self,anchor,position):
        self.job_table.doItemsLayout()
        # Rebuilt trees defer their range calculation; restore after layout too.
        self.job_table.verticalScrollBar().setValue(position)
        generation=getattr(self,'_scroll_generation',0)+1;self._scroll_generation=generation
        def restore():
            if self._scroll_generation!=generation:return
            self.job_table.doItemsLayout();item=None
            if anchor:
                identity,role,child=anchor;item=self.job_items.get(identity) if role=='job' else self.job_groups.get(role)
                if item and child is not None and child<item.childCount():item=item.child(child)
            if item:self.job_table.scrollToItem(item,QAbstractItemView.PositionAtTop)
            else:self.job_table.verticalScrollBar().setValue(position)
        QTimer.singleShot(0,restore)
    def change_task_filter(self):
        self.job_offset=0;self.refresh()
    def change_task_view(self):
        self.clear_checked_jobs();self.change_task_filter()
    def all_task_action(self,action):
        if getattr(self,'all_action_pending',False):return
        if action=='ai' and not self.config.get('ai',{}).get('enabled'):
            self.notice.setText('请先在连接与设置中启用 AI，并填写服务地址、模型和 API Key。');return
        if action=='ai':
            self.call('queue_ai_reviews',{'all':True,'apply_verified':True},lambda result:(self.notice.setText(result.get('message') or '已登记后台核对队列'),self.refresh()),lambda message:self.notice.setText(message));return
        self.all_action_pending=True;self.notice.setText('正在整理全部任务…')
        states=['failed'] if action=='retry' else ['review','failed'];ids=[]
        def collect():
            if states:
                state=states.pop(0)
                self.call('list_workflows',{'state':state,'selection':True,'view':self.task_view.currentData()},lambda result:(ids.extend(result.get('job_ids',[])),collect()),failed);return
            ids[:]=list(dict.fromkeys(ids))
            if action=='manual':
                self.all_action_pending=False;self.manual_review_queue=ids;self.next_manual_review();return
            process()
        done=[0];problems=[]
        def process():
            if not ids:
                self.all_action_pending=False;self.notice.setText(('AI 核对' if action=='ai' else '重试')+'已处理 '+str(done[0])+' 项'+('；部分任务需手动处理' if problems else ''));self.refresh();return
            size=500;chunk=ids[:size];del ids[:size]
            self.notice.setText('正在处理全部任务：'+str(done[0])+' 项完成，剩余 '+str(len(ids)+len(chunk))+' 项')
            def received(result):
                done[0]+=len(chunk)
                if result.get('error'):problems.append(result['error'])
                process()
            self.call('assist_workflows' if action=='ai' else 'bulk_workflows',{'job_ids':chunk,**({'action':'retry'} if action=='retry' else {'apply_verified':True})},received,failed)
        def failed(message):self.all_action_pending=False;self.notice.setText('全量操作中断：'+message)
        collect()
    def next_manual_review(self):
        queue=getattr(self,'manual_review_queue',[])
        if not queue:self.notice.setText('本轮手动核对已完成');self.refresh();return
        job_id=queue.pop(0);self.notice.setText('连续核对：还剩 '+str(len(queue)+1)+' 张；保存后自动提交并进入下一张，关闭窗口则停止本轮核对。')
        def received(result):
            job=result['job']
            if job.get('stage') not in ('needs_review','failed','skipped'):self.next_manual_review();return
            self.review_dialog(job,lambda:QTimer.singleShot(0,self.next_manual_review))
        self.call('get_workflow',{'job_id':job_id},received,lambda _:self.next_manual_review())
    def job_page(self,delta):
        self.job_offset=max(0,min(max(0,(self.job_total-1)//200*200),self.job_offset+delta));self.refresh()
    def show_detail(self):
        job_id=self.selected_job();job=self.jobs.get(job_id)
        stage=(job or {}).get('stage');self.review_current.setEnabled(stage in ('needs_review','failed'));self.retry_current.setEnabled(stage in ('needs_review','failed','skipped'));self.restore_current.setEnabled(bool(job) and stage in ('completed','cancelled','failed','restored') and job.get('task_type') not in ('identification','search'))
        if job_id!=getattr(self,'_detail_job_id',None):self.detail_pinned=False;self._detail_job_id=job_id
        if job and not getattr(self,'detail_pinned',False):
            chain=' → '.join({'identification':'识别','search':'搜索','album':'下载与入库'}.get(r.get('task_type'),'任务')+'：'+STAGES.get(r['stage'],r['stage']) for r in job.get('chain',[]))
            self.job_detail.setPlainText((chain+'\n' if chain else '')+self.job_detail_notes.get(job_id,job.get('message','')+'\n'+job.get('detail','')))
    def cancel(self):
        self.bulk_action('cancel')
    def bulk_action(self,action,job_ids=None):
        if self.bulk_pending or not (job_ids or self.checked_jobs):return
        ids=job_ids or list(self.checked_jobs)
        if action=='delete' and QMessageBox.question(self,'删除任务记录','删除选中的任务及后续任务记录？未完成的后续处理会取消；音乐文件、下载和做种均保留。')!=QMessageBox.Yes:return
        self.bulk_pending=True;self.detail_pinned=True;self.update_job_selection();names={'pause':'暂停','resume':'继续','cancel':'取消','retry':'重试','delete':'删除'};self.job_detail.setPlainText(f'正在{names[action]} {len(ids)} 项任务；操作在安全阶段生效。')
        def completed(result):
            self.bulk_pending=False;message=result.get('message') or f"{names[action]}：成功 {result.get('succeeded',0)} 项，跳过 {result.get('skipped',0)} 项，未完成 {result.get('failed',0)} 项";self.notice.setText(message)
            failures=[row for row in result.get('results',[]) if row.get('status')!='OK']
            if action=='delete':
                for row in result.get('results',[]):
                    for removed in row.get('removed_job_ids',[]):self.checked_jobs.pop(removed,None)
            self.job_detail.setPlainText(message+''.join('\n'+str(row.get('job_id',''))+'：'+str(row.get('message','')) for row in failures[:20]));self.update_job_selection();self.refresh()
        def failed(message):self.bulk_pending=False;self.job_detail.setPlainText('批量操作未完成：'+message);self.update_job_selection()
        self.call('bulk_workflows',{'action':action,'job_ids':ids},completed,failed)
    def selected_job(self):
        item=self.job_table.currentItem()
        if item and item.data(0,Qt.UserRole+1)=='track':item=item.parent()
        return item.data(0,Qt.UserRole) if item and item.data(0,Qt.UserRole+1)=='job' else None
    def explain(self):
        if not self.checked_jobs or getattr(self,'bulk_ai_pending',False):return
        self.detail_pinned=True
        if not self.config.get('ai',{}).get('enabled'):self.job_detail.setPlainText('可在“连接与设置”中启用 AI 并填写服务地址、模型和 API Key。');return
        ids=list(self.checked_jobs);self.bulk_ai_pending=True;self.update_job_selection();self.job_detail.setPlainText('正在登记 AI 辅助核对队列…')
        def received(result):
            self.bulk_ai_pending=False;self.update_job_selection()
            text=str(result.get('message') or result.get('error') or 'AI 辅助已完成')
            self.job_detail.setPlainText(text+''.join('\n'+str(row.get('label') or row.get('job_id'))+'：'+str(row.get('message','')) for row in result.get('results',[])));self.refresh()
        def failed(message):self.bulk_ai_pending=False;self.update_job_selection();self.job_detail.setPlainText('AI 辅助未完成：'+message)
        self.call('queue_ai_reviews',{'job_ids':ids,'apply_verified':True},received,failed)
    def restore(self):
        job_id=self.selected_job()
        if not job_id:return
        if self.jobs.get(job_id,{}).get('job_kind')=='batch':self.job_detail.setPlainText('批次记录不包含已替换文件。请选择具体专辑任务恢复原版。');return
        if QMessageBox.question(self,'恢复原版','恢复此任务保存的原版，并移出本任务新发布的文件？后来被改动的文件会拦截恢复；做种任务保留。正在处理的任务请先取消。')!=QMessageBox.Yes:return
        self.call('restore_workflow',{'job_id':job_id},lambda _:self.refresh())
    def review(self):
        job_id=self.selected_job()
        if job_id:self.call('get_workflow',{'job_id':job_id},lambda result:self.review_dialog(result['job']))
    def review_dialog(self,job,on_saved=None):
        if job.get('review_kind')=='token':self.token_prompt(job['job_id'],on_saved);return
        if job.get('job_kind')=='batch':
            if job.get('stage') in ('failed','cancelled','needs_review'):
                QMessageBox.information(self,'重新开始任务准备','上次准备已中断，请返回对应入口重新开始。本记录不会自动执行。');self.nav.setCurrentRow({'playlist':0,'complete':1,'upgrade':2}.get(job.get('scene'),3))
            else:QMessageBox.information(self,'批次准备记录','正在准备时可查看扫描进度或取消本批次。准备完成后，请选择具体的专辑任务进行核对。')
            return
        if job.get('review_kind') in ('metadata','candidate','search','error','interrupted') or job.get('stage') in ('failed','skipped'):
            from musicops.gui.review import ReviewDialog
            self.review_window=ReviewDialog(self,job,on_saved=on_saved);self.review_window.open();return
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
        self.call('review_workflow',payload,lambda _:(self.refresh(),on_saved() if on_saved else None))
    def token_prompt(self,job_id,on_saved=None):
        self.token_prompt_open=True
        def received(result):
            job=result['job']
            if job.get('stage')!='needs_review' or job.get('review_kind')!='token':self.token_prompt_open=False;return
            prompt=QMessageBox(self);prompt.setWindowTitle('FL 令牌下载未通过');prompt.setText(job.get('label','')+'\n'+job.get('detail',''))
            prompt.setInformativeText('默认仅使用令牌免费方式。普通下载可能计入站点下载量，且不消耗 FL。只有在此明确选择才授权；本批次授权不会保存为以后任务的默认设置。')
            retry=prompt.addButton('重新检查并使用 FL 令牌',QMessageBox.ActionRole);once=prompt.addButton('仅此次普通下载',QMessageBox.ActionRole);batch=prompt.addButton('本批次全部普通下载',QMessageBox.ActionRole);wait=prompt.addButton('保持令牌下载，暂不下载',QMessageBox.RejectRole);prompt.setDefaultButton(wait);prompt.setEscapeButton(wait)
            def chosen(_):
                self.token_prompt_open=False;clicked=prompt.clickedButton()
                if clicked==retry:
                    self.call('bulk_workflows',{'action':'retry','job_ids':[job_id]},lambda result:(self.notice.setText(result.get('message','')),self.refresh(),on_saved() if on_saved and result.get('succeeded') else None));return
                if clicked not in (once,batch):return
                payload={'job_id':job_id,'choice':'nonfree_once' if clicked==once else 'nonfree_batch','expected_revision':job.get('review_revision')}
                self.call('review_workflow',payload,lambda _:(self.refresh(),on_saved() if on_saved else None))
            prompt.finished.connect(chosen);self.token_window=prompt;prompt.open()
        self.call('get_workflow',{'job_id':job_id},received,lambda message:(setattr(self,'token_prompt_open',False),self.notice.setText(message)))
    def closeEvent(self,event):self.timer.stop();self.task_query_timer.stop();event.accept()

STYLE='''
QWidget#workbenchPage, QScrollArea#pageScroll { background: #0d1523; border: 0; }

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
QComboBox { combobox-popup: 0; }
QComboBox QAbstractItemView { background: #14243a; color: #ecf5ff; selection-background-color: #24485a; border: 1px solid #34465f; padding: 4px 0; outline: 0; }
QComboBox QAbstractItemView::item { min-height: 30px; padding: 5px 12px; border: 0; }
QComboBox QAbstractItemView::item:hover { background: #203d50; }
QComboBox:focus { border-color: #64dcca; } QRadioButton { color: #c1d4e9; spacing: 9px; padding: 6px 0; }
QLineEdit:hover,QPlainTextEdit:hover,QSpinBox:hover { border-color: #55728e; }
QLineEdit:focus,QPlainTextEdit:focus,QSpinBox:focus { border: 1px solid #64dcca; background: #14243a; }
QLineEdit:disabled,QSpinBox:disabled { color: #64758d; background: #101827; border-color: #26354a; }
QSpinBox::up-button,QSpinBox::down-button { width: 20px; background: #22364e; border: 0; border-radius: 3px; }
QListWidget { background: transparent; border: 0; outline: 0; font-size: 14px; }
QListWidget::item { padding: 15px 12px; border-radius: 9px; color: #a6bad2; border: 1px solid transparent; }
QListWidget::item:hover { background: #1a2c43; color: #eef6ff; }
QListWidget::item:selected { background: #1b3b44; color: #9df2dd; border-color: #2c6062; font-weight: 600; }
QTableWidget,QTreeWidget { background: #121f32; alternate-background-color: #16253a; border: 1px solid #2b3e56; border-radius: 10px; selection-background-color: #24485a; selection-color: #f0fffb; outline: 0; }
QHeaderView::section { background: #1d3048; color: #9eb7d1; padding: 13px 12px; border: 0; border-bottom: 1px solid #32445d; font-weight: 600; font-size: 12px; }
QTableWidget::item { padding: 10px; border-bottom: 1px solid #1e3045; }
QTreeWidget::item { padding: 7px 5px; border-bottom: 1px solid #1e3045; min-height: 19px; }
QTableWidget::item:selected,QTreeWidget::item:selected { background: #24485a; }
QCheckBox { spacing: 10px; color: #c1d4e9; min-height: 24px; }
QCheckBox::indicator,QTableWidget::indicator,QTreeWidget::indicator { width: 17px; height: 17px; border: 1px solid #617d98; background: #14243a; border-radius: 4px; }
QCheckBox::indicator:checked,QTableWidget::indicator:checked,QTreeWidget::indicator:checked { background: #5cddc5; border: 2px solid #a0f4e4; image: none; }
QCheckBox::indicator:indeterminate,QTreeWidget::indicator:indeterminate { background: #357d7b; border: 2px solid #78beb5; }
QCheckBox::indicator:hover,QTableWidget::indicator:hover,QTreeWidget::indicator:hover { border-color: #8be9d7; }
QProgressBar#activityProgress { background: #1d2c41; border: 0; border-radius: 1px; }
QProgressBar#activityProgress::chunk { background: #4dcab9; border-radius: 1px; }
QTabWidget::pane { border: 1px solid #2b3e56; border-radius: 8px; background: #0d1523; }
QTabBar::tab { background: #14243a; color: #9eb7d1; border: 1px solid #2b3e56; padding: 10px 20px; margin-right: 4px; border-top-left-radius: 7px; border-top-right-radius: 7px; }
QTabBar::tab:selected { background: #1b3b44; color: #9df2dd; border-bottom-color: #1b3b44; }
QScrollArea { border: 0; }
QScrollBar:vertical { width: 9px; background: transparent; margin: 3px; }
QScrollBar::handle:vertical { background: #3b526b; min-height: 36px; border-radius: 3px; }
QScrollBar::handle:vertical:hover { background: #5d8199; }
QScrollBar::add-line:vertical,QScrollBar::sub-line:vertical { height: 0; }
QScrollBar::add-page:vertical,QScrollBar::sub-page:vertical { background: transparent; }
QToolTip { color: #e8f6ff; background: #20354c; border: 1px solid #58718b; padding: 7px; }
'''
