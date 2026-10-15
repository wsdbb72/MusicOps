"""Three user workflows and a skippable connection wizard."""
from __future__ import annotations
import copy,json,os
from pathlib import Path
from PySide6.QtCore import QObject,QRunnable,QThreadPool,QTimer,Qt,Signal
from PySide6.QtGui import QFont,QDesktopServices
from PySide6.QtCore import QUrl
from PySide6.QtWidgets import (QMainWindow,QWidget,QVBoxLayout,QHBoxLayout,QLabel,QPushButton,QLineEdit,QPlainTextEdit,QFileDialog,QCheckBox,QGroupBox,QFormLayout,QWizard,QWizardPage,QListWidget,QStackedWidget,QTableWidget,QTableWidgetItem,QHeaderView,QMessageBox,QSpinBox,QScrollArea,QInputDialog,QDialog,QDialogButtonBox)
from musicops.worker.client import WorkerClient
from musicops.clients.http import redact

SITES=[('open.cd','OpenCD','www.open.cd'),('dicmusic','DicMusic','dicmusic.com'),('orpheus','Orpheus','orpheus.network'),('redacted','RED','redacted.sh')]
SCENES={'playlist':'歌单入库','complete':'资料补全','upgrade':'音质升级'}
STAGES={'restored':'已恢复原版','enrich':'补全资料','planning':'分析曲目','searching':'寻找音源','requesting':'获取下载资源','downloading':'下载中','fallback':'网易云下载','metadata':'整理专辑资料','validating':'检查完整音频','publishing':'写入音乐库','retiring':'移出旧版','notifying':'通知音乐服务器','running':'正在处理','completed':'已完成','needs_review':'需要核对','failed':'未完成','cancelled':'已取消','skipped':'已跳过','waiting':'等待处理'}

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
    w=QLabel(text);w.setWordWrap(True)
    if kind:w.setObjectName(kind)
    return w
def button(text,callback,primary=False):
    w=QPushButton(text);w.clicked.connect(callback)
    if primary:w.setObjectName('primary')
    return w
def browse_field(value,folder=True):
    root=QWidget();box=QHBoxLayout(root);box.setContentsMargins(0,0,0,0);edit=line(value)
    def choose():
        path=QFileDialog.getExistingDirectory(root,'选择目录',edit.text()) if folder else QFileDialog.getOpenFileName(root,'选择程序',edit.text(),'程序 (*.exe);;所有文件 (*)')[0]
        if path:edit.setText(path)
    box.addWidget(edit);box.addWidget(button('选择…',choose));return root,edit

class SetupWizard(QWizard):
    def __init__(self,owner):
        super().__init__(owner);self.owner=owner;self.values=copy.deepcopy(owner.config);self.setWindowTitle('设置 MusicOps');self.setWizardStyle(QWizard.ModernStyle);self.resize(830,680)
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
            help_text='已保存登录信息；留空即可保留。' if settings.get('credential_ref') in self.saved_refs else '登录网站 → Cookie-Editor → 导出 JSON → 导入文件或粘贴。'
            layout.addWidget(label(help_text,'hint'));cookie=QPlainTextEdit();cookie.setPlaceholderText('粘贴 Cookie-Editor 导出的 JSON（不会显示已保存的 Cookie）');cookie.setMaximumHeight(64);layout.addWidget(cookie)
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
        cfg=self.values.get('qbt',{});self.qbt_url=line(cfg.get('base_url','http://127.0.0.1:8080'),'例如 http://127.0.0.1:8080');self.qbt_user=line('','WebUI 用户名');self.qbt_pass=line('','WebUI 密码；留空保留已保存密码',True)
        p.addRow('WebUI 地址',self.qbt_url);p.addRow('用户名',self.qbt_user);p.addRow('密码',self.qbt_pass)
        p.addRow(label('已保存 qBittorrent 登录信息；留空保留。' if cfg.get('credential_ref') in self.saved_refs else '没有 qBittorrent 时可跳过；资料补全仍可使用。','hint'))
        self.fl_limit=QSpinBox();self.fl_limit.setRange(0,100);self.fl_limit.setValue(self.values.get('workflow',{}).get('fl_per_batch',3));self.fl_limit.setSuffix(' 次 / 批次');p.addRow('最多使用 FL 免费下载',self.fl_limit)
        p.addRow(label('OpenCD 直接获取种子；其他站点在需要时使用 FL。用尽额度后保留为待处理，不使用付费下载。','hint'))
        p=self._add_page('网易云回退与媒体工具','四站没有合适音源时使用网易云。无法获取的曲目会保留说明，不会用其他歌曲冒充。')
        flow=self.values.get('workflow',{});self.fallback=QCheckBox('启用网易云回退');self.fallback.setChecked(flow.get('netease_fallback',True));p.addRow(self.fallback)
        self.netease_api=line(flow.get('netease_api_url',''),'可选：本地网易云 API 服务地址');p.addRow('API 服务（可选）',self.netease_api)
        row,self.import_dir=browse_field(flow.get('netease_import_dir',''));p.addRow('客户端下载目录（可选）',row)
        self.netease_cookie=QPlainTextEdit();self.netease_cookie.setMaximumHeight(64);self.netease_cookie.setPlaceholderText('可选：网易云 Cookie-Editor JSON；留空保留已保存的登录态');p.addRow('网易云登录态（可选）',self.netease_cookie)
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
        p=self._add_page('补全封面、歌词与资料','这些服务都可以跳过。自动写入歌词时保存为音频同名的 .lrc 文件；没有歌词的曲目不会被视为损坏。')
        lyrics=self.values.get('lyrics',{}).get('primary',{});self.lyric_url=line('' if '.example.invalid' in lyrics.get('base_url','') else lyrics.get('base_url',''),'歌词与封面接口地址');self.lyric_key=line('','填写服务提供的完整 Authorization 值；留空保留',True)
        p.addRow('歌词接口地址',self.lyric_url);p.addRow('接口鉴权值',self.lyric_key);p.addRow(label('这里填服务给你的鉴权原文，无需填写“凭据引用”。没有服务可留空。','hint'))
        self.discogs=line('','Discogs 设置中生成的个人 Token；可跳过',True);p.addRow('Discogs Token',self.discogs)
        ai=self.values.get('ai',{});self.ai_enabled=QCheckBox('使用 AI 辅助解释待核对项目');self.ai_enabled.setChecked(ai.get('enabled',False));p.addRow(self.ai_enabled)
        self.ai_url=line('' if '.example.invalid' in ai.get('base_url','') else ai.get('base_url',''),'兼容 Responses 的 API 地址');self.ai_model=line(ai.get('model',''),'服务提供的模型名称');self.ai_key=line('','API Key；留空保留已保存密钥',True)
        p.addRow('AI 地址',self.ai_url);p.addRow('模型名称',self.ai_model);p.addRow('API Key',self.ai_key)
        p=self._add_page('连接 Navidrome（可选）','程序完成入库后会触发 Navidrome 扫描。若只使用 Roon，可直接保存。')
        nav=self.values.get('music_servers',{}).get('navidrome',{});self.nav_enabled=QCheckBox('启用 Navidrome');self.nav_enabled.setChecked(nav.get('enabled',False));p.addRow(self.nav_enabled)
        self.nav_url=line(nav.get('url',''),'例如 http://127.0.0.1:4533');self.nav_user=line(nav.get('username',''),'具有扫描权限的账号');self.nav_pass=line('','账号密码；留空保留',True)
        p.addRow('服务地址',self.nav_url);p.addRow('用户名',self.nav_user);p.addRow('密码',self.nav_pass)
        p.addRow(label('Navidrome 需要监听与你选择的音乐库相同的文件。密码只在当前 Windows 账户的本地保险库中保存。','hint'))
        self.save_note=label('');p.addRow(self.save_note)
    def _add_page(self,title,subtitle):
        page=QWizardPage();page.setTitle(title);page.setSubTitle(subtitle);outer=QVBoxLayout(page);scroll=QScrollArea();scroll.setWidgetResizable(True);scroll.setFrameShape(QScrollArea.NoFrame);body=QWidget();form=QFormLayout(body);form.setVerticalSpacing(14);scroll.setWidget(body);outer.addWidget(scroll);self.addPage(page);return form
    def accept(self):
        payload={'library':self.library.text().strip(),'netease_cookie_json':self.netease_cookie.toPlainText().strip(),'sites':{k:{'enabled':e.isChecked(),'cookie_json':t.toPlainText().strip()} for k,(e,t) in self.site_fields.items()},
                 'qbt':{'url':self.qbt_url.text().strip(),'username':self.qbt_user.text().strip(),'password':self.qbt_pass.text()},
                 'lyrics':{'url':self.lyric_url.text().strip(),'authorization':self.lyric_key.text()},'discogs_token':self.discogs.text(),
                 'ai':{'enabled':self.ai_enabled.isChecked(),'url':self.ai_url.text().strip(),'model':self.ai_model.text().strip(),'key':self.ai_key.text()},
                 'servers':{'roon':self.roon.isChecked(),'navidrome':{'enabled':self.nav_enabled.isChecked(),'url':self.nav_url.text().strip(),'username':self.nav_user.text().strip(),'password':self.nav_pass.text()}},
                 'workflow':{'netease_fallback':self.fallback.isChecked(),'netease_api_url':self.netease_api.text().strip(),'netease_import_dir':self.import_dir.text().strip(),'fl_per_batch':self.fl_limit.value()},
                 **{k:w.text().strip() for k,w in self.fields.items()}}
        self.button(QWizard.FinishButton).setEnabled(False);self.save_note.setText('正在保存…')
        def saved(result):self.owner.config=result['config']['values'];super(SetupWizard,self).accept();self.owner.refresh()
        def failed(message):self.button(QWizard.FinishButton).setEnabled(True);self.save_note.setText(message)
        self.owner.call('save_setup',payload,saved,failed)

class Workbench(QMainWindow):
    def __init__(self,base,client=None,autostart=True):
        super().__init__();self.base=Path(base);self.client=client or WorkerClient(base);self.config={};self.tasks={};self.jobs={};self.targets=[];self.requests=set();self.pool=QThreadPool(self);self.setWindowTitle('MusicOps');self.resize(1220,820);self.setMinimumSize(1000,700)
        container=QWidget();self.setCentralWidget(container);layout=QHBoxLayout(container);layout.setContentsMargins(0,0,0,0);layout.setSpacing(0)
        side=QWidget();side.setObjectName('sidebar');side.setFixedWidth(206);s=QVBoxLayout(side);s.setContentsMargins(24,30,20,22);s.addWidget(label('MusicOps','brand'));s.addWidget(label('让音乐有序入库','muted'));s.addSpacing(28)
        self.nav=QListWidget();self.nav.addItems(['歌单入库','资料补全','音质升级','任务中心']);s.addWidget(self.nav);s.addWidget(button('连接与设置',self.setup));s.addWidget(label('音乐库管理','muted'));layout.addWidget(side)
        content=QWidget();c=QVBoxLayout(content);c.setContentsMargins(30,28,30,24);self.connection=label('正在连接后台…','connection');c.addWidget(self.connection)
        self.notice=label('','notice');c.addWidget(self.notice);self.stack=QStackedWidget();c.addWidget(self.stack);layout.addWidget(content);self.nav.currentRowChanged.connect(self.stack.setCurrentIndex)
        self._playlist();self._complete();self._upgrade();self._tasks();self.nav.setCurrentRow(0)
        self.timer=QTimer(self);self.timer.setInterval(2000);self.timer.timeout.connect(self.refresh);self.wizard_shown=False;self.job_offset=0;self.job_total=0
        if autostart:self.call('attach',{},lambda r:self.call('enable_online',{},lambda _:self.refresh()))
    def page(self,title,subtitle):
        w=QWidget();box=QVBoxLayout(w);box.setContentsMargins(0,14,0,0);box.setSpacing(18);box.addWidget(label(title,'title'));box.addWidget(label(subtitle,'subtitle'));self.stack.addWidget(w);return box
    def steps(self,box,text):
        group=QGroupBox();group.setObjectName('card');layout=QVBoxLayout(group);layout.addWidget(label(text,'steps'));box.addWidget(group)
    def table(self,columns):
        table=QTableWidget(0,len(columns));table.setHorizontalHeaderLabels(columns);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch);table.verticalHeader().hide();table.verticalHeader().setDefaultSectionSize(42);table.setAlternatingRowColors(True);table.setSelectionBehavior(QTableWidget.SelectRows);table.setEditTriggers(QTableWidget.NoEditTriggers);table.setShowGrid(False);return table
    def _playlist(self):
        box=self.page('把歌单带进音乐库','粘贴网易云歌单，自动查找缺少的音乐、整理资料，并交给 Roon / Navidrome。');self.steps(box,'解析歌单  →  四站检索  →  下载  →  补齐资料与校验  →  入库')
        row=QHBoxLayout();self.playlist=line('','粘贴网易云歌单链接或 ID');row.addWidget(self.playlist);row.addWidget(button('解析歌单',self.parse,True));box.addLayout(row)
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
        self.job_table=self.table(['任务','内容','当前步骤','进度','结果']);box.addWidget(self.job_table);self.job_detail=label('选择一项查看结果或需要补充的信息。','hint');box.addWidget(self.job_detail);self.job_table.itemSelectionChanged.connect(self.show_detail)
        box.addWidget(button('AI 解释选中问题（可选）',self.explain))
        box.addWidget(button('恢复选中任务的原版',self.restore))
        pages=QHBoxLayout();self.job_counts=label('');pages.addWidget(self.job_counts);pages.addStretch();pages.addWidget(button('上一页',lambda:self.job_page(-200)));pages.addWidget(button('下一页',lambda:self.job_page(200)));box.addLayout(pages)
    def setup(self):
        if not self.config:return
        self.wizard=SetupWizard(self);self.wizard.open()
    def call(self,command,payload,done=None,failed=None):
        request=Request(self.client,command,payload);self.requests.add(request)
        def success(result):
            self.requests.discard(request)
            if result.get('accepted') and done:self.tasks[result['task_id']]=done
            elif done:done(result)
        def failure(message):
            self.requests.discard(request)
            if failed:failed(message)
            else:self.notice.setText('需要处理：'+message)
        request.signals.done.connect(success);request.signals.failed.connect(failure);self.pool.start(request)
    def refresh(self):
        if len(self.requests)>3:return
        self.call('get_status',{'compact':True},self.status)
        self.call('list_workflows',{'offset':self.job_offset},self.show_jobs)
    def status(self,result):
        self.config=result['config']['values'];self.available_refs=[r['ref'] if isinstance(r,dict) else r for r in result.get('secret_refs',[])];self.timer.start();connected=[name for key,name,_ in SITES if self.config['providers'][key]['enabled'] and self.config['providers'][key]['credential_ref'] in self.available_refs]
        self.connection.setText('音乐库：'+self.config['paths']['library']+'     ·     '+('已配置 '+', '.join(connected) if connected else '尚未连接资源站；可在“连接与设置”添加'))
        if not self.wizard_shown and not self.config['workflow']['setup_done']:
            self.wizard_shown=True;QTimer.singleShot(200,self.setup)
        for task in result.get('tasks',[]):
            tid=task['task_id']
            if tid in self.tasks and task['state'] in ('completed','failed','blocked','cancelled','recovery_required'):
                callback=self.tasks.pop(tid)
                def received(response,cb=callback):
                    task=response['task'];result=task.get('result') or {'error':task.get('error','任务未完成')}
                    if task['state']!='completed':self.notice.setText('任务未完成：'+str(task.get('error') or result.get('reason') or task['state']))
                    cb(result)
                self.call('get_task',{'task_id':tid},received)
    def parse(self):
        self.playlist_note.setText('正在读取歌单…')
        def parsed(result):
            if result.get('error'):self.playlist_note.setText(result['error']);return
            self.loaded_tracks({'targets':result.get('targets',[]),'complete':result.get('complete',False)})
        self.call('refresh_playlist',{'playlist_input':self.playlist.text().strip()},parsed)
    def loaded_tracks(self,result):
        self.targets=result['targets'];self.track_table.setRowCount(len(self.targets))
        self.track_table.setUpdatesEnabled(False)
        for i,target in enumerate(self.targets):
            check=QTableWidgetItem();check.setFlags(Qt.ItemIsUserCheckable|Qt.ItemIsEnabled);check.setCheckState(Qt.Unchecked);check.setData(Qt.UserRole,target['target_id']);self.track_table.setItem(i,0,check)
            for j,value in enumerate((target['raw_title'],' / '.join(target['artists']),target.get('album') or '待识别','待检查音乐库' if target.get('metadata_complete',True) else '资料缺失，需识别'),1):self.track_table.setItem(i,j,QTableWidgetItem(value))
        self.track_table.setUpdatesEnabled(True)
        self.playlist_note.setText(f'已载入 {len(self.targets)} 首；选择需要入库的曲目。任务会按专辑去重。'+(' 来源清单未完全对账，仅处理你明确选择的可见曲目。' if not result.get('complete',True) else ''))
    def select_all(self):
        checked=not all(self.track_table.item(i,0).checkState()==Qt.Checked for i in range(self.track_table.rowCount()))
        for i in range(self.track_table.rowCount()):self.track_table.item(i,0).setCheckState(Qt.Checked if checked else Qt.Unchecked)
    def start(self,scene,refresh_resources=False):
        if not self.config.get('workflow',{}).get('setup_done'):self.setup();return
        targets=[self.track_table.item(i,0).data(Qt.UserRole) for i in range(self.track_table.rowCount()) if self.track_table.item(i,0).checkState()==Qt.Checked] if scene=='playlist' else []
        if scene=='playlist' and not targets:self.playlist_note.setText('请先选择要入库的曲目。');return
        self.notice.setText('正在检查音乐库并创建专辑任务…');self.nav.setCurrentRow(3)
        def started(result):
            if result.get('error'):self.notice.setText('任务未创建：'+result['error'])
            else:self.notice.setText(result.get('message','任务已创建'))
            self.refresh()
        self.call('start_workflow',{'scene':scene,'target_ids':targets,'prefer_24bit':self.prefer24.isChecked(),'refresh_resources':refresh_resources},started)
    def show_jobs(self,result):
        self.job_total=result.get('total',0);self.job_counts.setText(f"共 {self.job_total} 项 · 运行 {result.get('active',0)} · 完成 {result.get('finished',0)} · 第 {self.job_offset//200+1} 页")
        selected=self.selected_job();jobs=result.get('jobs',[]);self.jobs={j['job_id']:j for j in jobs};self.job_table.blockSignals(True);self.job_table.setRowCount(len(jobs))
        for i,job in enumerate(jobs):
            values=(SCENES.get(job['scene'],job['scene']),job.get('label','音乐库'),STAGES.get(job['stage'],job['stage']),str(job.get('progress',0))+'%',job.get('message',''))
            for j,value in enumerate(values):item=QTableWidgetItem(str(value));item.setData(Qt.UserRole,job['job_id']);self.job_table.setItem(i,j,item)
            if job['job_id']==selected:self.job_table.selectRow(i)
        self.job_table.blockSignals(False)
        self.show_detail()
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
        if QMessageBox.question(self,'恢复原版','恢复此任务保存的原版，并移出本任务新发布的文件？后来被改动的文件会拦截恢复；做种任务保留。正在处理的任务请先取消。')!=QMessageBox.Yes:return
        self.call('restore_workflow',{'job_id':job_id},lambda _:self.refresh())
    def review(self):
        job_id=self.selected_job()
        if job_id:self.call('get_workflow',{'job_id':job_id},lambda result:self.review_dialog(result['job']))
    def review_dialog(self,job):
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
            layout.addLayout(links);rows=job.get('prepared') or [];table=QTableWidget(len(rows),3);table.setHorizontalHeaderLabels(['歌曲','艺人','专辑']);table.horizontalHeader().setSectionResizeMode(QHeaderView.Stretch)
            for i,row in enumerate(rows):
                for j,value in enumerate((row.get('raw_title',''),' / '.join(row.get('artists',[])),row.get('album',''))):table.setItem(i,j,QTableWidgetItem(value))
            layout.addWidget(table);buttons=QDialogButtonBox(QDialogButtonBox.Save|QDialogButtonBox.Cancel);buttons.accepted.connect(dialog.accept);buttons.rejected.connect(dialog.reject);layout.addWidget(buttons)
            if dialog.exec()!=QDialog.Accepted:return
            payload['metadata']=[{k:table.item(i,j).text() for j,k in enumerate(('title','artist','album'))} for i in range(len(rows))]
        elif kind in ('search','error','interrupted'):
            if QMessageBox.question(self,'继续处理','确认已处理连接或工具问题后继续。已有下载会先核对，不会重复请求未知状态的 FL。')!=QMessageBox.Yes:return
            payload['choice']='retry'
        else:
            QMessageBox.information(self,'需要补充资料',job.get('message','')+'\n'+job.get('detail',''));return
        self.call('review_workflow',payload,lambda _:self.refresh())
    def closeEvent(self,event):self.timer.stop();event.accept()

STYLE='''
QWidget { font-family: "Microsoft YaHei"; font-size: 13px; color: #263746; }
QMainWindow, QWizard { background: #f5f7f9; } QWidget#sidebar { background: #fff; border-right: 1px solid #e4e9ec; }
QLabel#brand { font-size: 28px; font-weight: 700; color: #167b76; } QLabel#title { font-size: 26px; font-weight: 700; color: #172b36; }
QLabel#subtitle { color: #667b87; font-size: 14px; } QLabel#muted,QLabel#hint { color: #72848e; } QLabel#connection { color: #526e79; font-size: 12px; }
QLabel#steps { color: #337e7a; font-size: 14px; } QGroupBox { border: 1px solid #e0e7ec; border-radius: 12px; background: #fff; padding: 18px; margin-top: 12px; }
QGroupBox::title { subcontrol-origin: margin; left: 18px; padding: 0 6px; font-weight: 600; }
QPushButton { background: #fff; border: 1px solid #dbe4e9; border-radius: 8px; padding: 10px 18px; color: #395463; }
QPushButton:hover { background: #edf5f4; border-color: #a8cdc9; } QPushButton#primary { background: #16857d; color: white; border: 0; font-weight: 600; }
QPushButton#primary:hover { background: #126f69; } QLineEdit,QPlainTextEdit,QSpinBox { background: #fff; border: 1px solid #dbe4e9; border-radius: 7px; padding: 9px; selection-background-color: #c1e4e1; }
QListWidget { background: transparent; border: 0; outline: 0; font-size: 15px; } QListWidget::item { padding: 14px 12px; margin: 3px 0; border-radius: 8px; } QListWidget::item:selected { background: #e3f2ef; color: #167b76; font-weight: 600; }
QTableWidget { background: #fff; alternate-background-color: #fafcfd; border: 1px solid #e4e9ec; border-radius: 10px; selection-background-color: #e3f2ef; selection-color: #263746; }
QHeaderView::section { background: #f3f7f9; color: #637985; padding: 12px; border: 0; font-weight: 600; } QTableWidget::item { padding: 9px; } QCheckBox { spacing: 9px; }
'''
