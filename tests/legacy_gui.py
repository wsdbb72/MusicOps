from __future__ import annotations
import argparse
import base64
import copy
import json
import os
import subprocess
import sys
from pathlib import Path
from urllib.parse import urlsplit
from PySide6.QtCore import QObject, QRunnable, QThreadPool, QTimer, Qt, Signal, QUrl
from PySide6.QtGui import QDesktopServices,QFont,QFontDatabase,QPixmap
from PySide6.QtWidgets import (QApplication,QCheckBox,QComboBox,QDoubleSpinBox,QFileDialog,QFormLayout,QGroupBox,QHBoxLayout,QLabel,QLineEdit,QListWidget,QMainWindow,QMessageBox,QPlainTextEdit,QPushButton,QScrollArea,QSpinBox,QSplitter,QStackedWidget,QTableWidget,QTableWidgetItem,QTabWidget,QVBoxLayout,QWidget)
from musicops.media.resources import shift_lrc, validate_lrc
from musicops.clients.http import redact
from musicops.worker.client import WorkerRequestError,WorkerConnectionError
from musicops import __version__

PAGE_NAMES=['总览','AI 助手','歌单与补齐','在线识别','资源搜索','下载与保种','音乐库与音质','库体检与修复','歌词与封面','复核中心','事务与回滚','配置','日志与报告']
STATE_LABELS={'verified_present':'已存在且确证','missing':'明确缺失','ambiguous':'待确认','version_mismatch':'版本不符','source_unavailable':'来源不可用','snapshot_incomplete':'快照不完整','queued':'排队','running':'运行中','paused':'已暂停','completed':'完成','blocked':'受阻','cancelled':'取消','failed':'失败','recovery_required':'需要恢复','pending':'Roon待复核','verified':'Roon显示通过','POLICY_BLOCKED':'现场策略阻断','AUTH_FAILED':'鉴权失败','VALID_PAYLOAD':'内容有效','NO_MATCH':'无匹配资源'}
STATE_LABELS.update(COMMITTED='已提交',ROLLED_BACK='已回滚',CONFLICT='外部改动冲突',BACKED_UP='原件已备份',STAGED='副本已暂存',PUBLISHED='已发布待提交',PREPARED='已准备')
HEADERS={'title':'标题','raw_title':'原始标题','artist':'艺人','artists':'艺人','album':'专辑','albumartist':'专辑艺人','state':'状态','main_state':'匹配状态','command':'操作','progress':'进度 %','source':'来源','provider':'资源来源','path':'文件位置','source_path':'来源位置','file_id':'文件标识','target_id':'曲目标识','candidate_id':'候选标识','resource_id':'资源标识','task_id':'任务标识','tx_id':'事务标识','config_version':'配置版本','created_at':'创建时间（UTC）','finished_at':'完成时间（UTC）','error_class':'错误分类','reason':'理由','evidence':'证据','kind':'资源类型','identity_state':'身份校验','validation':'校验结果','estimated_cost':'预计令牌','seeders':'做种数','eligibility':'资格','version':'录音版本','edition':'发行版本','before':'修改前','after':'修改后','field':'字段','locked':'已锁定','selected':'已选择','quarantine_file_id':'拟隔离文件','keep_file_id':'拟保留文件','action':'建议','recording_confirmed':'录音确证','release_confirmed':'发行确证','schema_version':'数据版本','size':'大小（字节）','read_error':'读取错误'}
PREFERRED_COLUMNS=['raw_title','title','artist','artists','album','state','main_state','command','progress','source','provider','kind','identity_state','field','before','after','action','reason','evidence','estimated_cost','seeders','quarantine_file_id','keep_file_id','tx_id','path','task_id','file_id']
LABELS={'offline':'离线模式','paths':'目录与工具','library':'主库','source':'来源区','work':'工作区','backup':'备份','quarantine':'隔离','reports':'报告','tools':'媒体工具','providers':'四个来源','token_budget':'令牌预算','qbt':'qBittorrent','lyrics':'歌词与封面','primary':'私有主服务','direct_backup':'直连HTTP备用','public_fallback':'公开HTTPS回退','quality':'音质偏好','metadata':'元数据规则','repair':'修复','dedup':'去重','scheduler':'调度','worker':'后台工作器','netease':'网易云','playlist_ids':'歌单ID','enabled':'启用','base_url':'服务地址','credential_ref':'凭据引用','auth_secret_ref':'密钥引用','search_language':'搜索语言','concurrency':'并发','timeout':'超时（秒）','rules_verified':'规则现场已确认','safety_margin':'安全余量','balance_ttl':'余额有效期（秒）','category':'分类','tag_prefix':'任务标签前缀','poll_seconds':'轮询（秒）','auto_tmm':'自动管理路径','lyrics_path':'歌词端点','candidates_path':'候选端点','cover_path':'封面端点','auth_mode':'鉴权方式','auth_header':'鉴权头名称','lyrics_url':'公开歌词地址','cover_url':'公开封面地址','fallback_on_auth_error':'鉴权失败后允许公开回退','write_mode':'资源写入方式','overwrite_existing':'已有资源策略','connect_timeout':'连接超时（秒）','read_timeout':'读取超时（秒）','max_attempts':'每源最大次数','total_budget':'每曲总预算（秒）','min_cover_side':'封面最短边（像素）','max_response_bytes':'响应字节上限','cache_ttl':'成功缓存TTL（秒）','negative_ttl':'无结果缓存TTL（秒）','lossless_first':'无损优先','format_order':'同身份格式顺序','prefer_24bit':'同母带偏好24位','dsd_policy':'DSD策略','multichannel':'多声道策略','upgrade_suggestions':'生成升级建议','preserve_multivalue':'保留多值','directory_template':'目录模板','track_padding':'轨号位数','preserve_unknown':'保留未知标签','overwrite':'覆盖策略','locked_fields':'锁定字段','mode':'模式','fields':'修复字段','backup_retention_days':'备份保留天数','same_release_required':'要求同发行','permanent_delete':'永久删除（禁用）','timezone':'业务时区','playlist_time':'每日歌单报告时间','health_weekday':'每周体检星期（0周一）','health_time':'体检时间','missed':'错过计划策略','notify':'通知策略','keep_background':'关闭界面后保留后台','decode_timeout':'全解码超时（秒）','batch_size':'歌单批次','strip_layers':'剥离的目录层级'}

LABELS.update({'offline':'演示档案（联网可在总览启用；不会自动改动音乐文件）','music_servers':'Roon / Navidrome','roon':'Roon','navidrome':'Navidrome','watched_folders':'Roon Storage监听目录（每行一项）','url':'Navidrome服务地址','username':'用户名','password_ref':'密码引用','client':'客户端名称','ai':'AI接口','protocol':'协议（Responses）','model':'模型名称','max_output_tokens':'最大输出token数','temperature':'温度','structured_outputs':'结构化计划输出','allow_plan_only':'允许生成审核计划','catalog':'在线识别','musicbrainz':'MusicBrainz','discogs':'Discogs','user_agent':'客户端标识','application_name':'应用名称','contact':'联系信息'})
LABELS.update(host='Core主机地址',port='Core连接端口',timeout='超时（秒）')
STATE_LABELS.update({'OK':'通过','AUTH_REQUIRED':'需配置凭据','NETWORK_ERROR':'网络失败','PARSE_ERROR':'解析失败','RATE_LIMITED':'站点限流','ACCESS_BLOCKED':'访问受阻','NO_RESOURCE':'没有匹配资源','DISABLED':'未启用','API_CONTRACT_ERROR':'接口结构不符'})
HEADERS.update({'format':'格式','encoding':'编码','edition':'发行版本','size_display':'大小','download_status':'下载资格'})
HEADERS.update({'confidence':'匹配排序（参考）','score':'来源匹配评分','release_title':'候选专辑','match_state':'匹配说明','match_reason':'匹配依据','verification':'复核状态','requires_manual_review':'需人工复核','requires_review':'需人工复核','identity_confirmed':'身份已确认','tracklist_complete':'完整曲目表','recording_id':'录音标识','release_id':'发行标识'})
STATE_LABELS['unknown']='待确认'
STATE_LABELS.update(SNAPSHOT_INCOMPLETE='歌单完整性未通过',PARTIAL='部分资源可用',IDENTITY_MISMATCH='身份不符',structural_only='仅内容结构通过',INVALID_INPUT='输入有误',INPUT_TOO_LARGE='内容过长',TRANSIENT_ERROR='暂时访问失败',VALIDATION_FAILED='校验未通过',REQUEST_UNKNOWN='请求结果待核对')
STATE_LABELS.update(mismatch='标题、艺人、专辑或版本不符',matched='身份已匹配',MISMATCH='资源身份不符',INVALID_PAYLOAD='资源内容无效')
STATE_LABELS.update(MANUAL_LOOKUP='官网人工核对',resolved='已读取完整证据',ranking_only='仅排序参考')
STATE_LABELS.update(PAIRING_REQUIRED='需要Roon配对',REQUIRES_PAIRING='需要Roon配对',NOT_CONFIRMED='尚未确证',CONNECTED='已连接')

def display_safe(value):
    if isinstance(value,dict):return {k:('[已隐藏]' if any(w in k.lower() for w in ('authorization','cookie','password','passkey','secret_value')) and not k.endswith('_ref') else display_safe(v)) for k,v in value.items() if k not in ('content_bytes','secret')}
    if isinstance(value,list):return [display_safe(x) for x in value]
    if isinstance(value,bytes):return f'[二进制资源 {len(value)} 字节]'
    return value

def pretty(value):return json.dumps(display_safe(redact(value)),ensure_ascii=False,indent=2,default=str)

class JobSignals(QObject):
    done=Signal(str,object)
    failed=Signal(str,object)

class IpcJob(QRunnable):
    def __init__(self,client,command,payload):
        super().__init__();self.client,self.command,self.payload=client,command,payload;self.signals=JobSignals()
    def run(self):
        try:
            if self.command=='attach':
                self.payload.setdefault('compact',True);attached=self.client.ensure_running()
                result=attached if isinstance(attached,dict) else self.client.call(self.command,self.payload)
            else:result=self.client.call(self.command,self.payload)
            self.signals.done.emit(self.command,result)
        except Exception as exc:
            # Exception text may contain remote data; present a stable local category.
            detail=str(exc) if isinstance(exc,RuntimeError) else '后台连接/请求失败，请查看脱敏日志并重新附着。'
            self.signals.failed.emit(self.command,{'message':f'{type(exc).__name__}：{redact(detail)}','error_class':getattr(exc,'error_class',type(exc).__name__),'connection_error':isinstance(exc,(OSError,EOFError,WorkerConnectionError))})
        finally:self.payload={}

class ConfigEditor(QWidget):
    """Every config leaf is a GUI control, preserving type and full snapshots."""
    save_requested=Signal(dict)
    def __init__(self):
        super().__init__();self.values={};self.controls={};self.version_number=0;self.tabs=QTabWidget();self.version=QLabel('等待配置')
        layout=QVBoxLayout(self);layout.addWidget(self.version);layout.addWidget(QLabel('保存只更新配置快照；运行中任务继续使用原版本。修改服务域名将解除旧凭据绑定。'))
        layout.addWidget(self.tabs)
        save=QPushButton('校验并保存全部配置');save.setObjectName('save_config');save.clicked.connect(self._save);layout.addWidget(save)
    def load(self,config):
        self.values=copy.deepcopy(config.get('values',{}));self.version_number=config.get('version',0);self.version.setText(f"档案：{config.get('config_id','default')}　配置版本：{self.version_number}")
        while self.tabs.count():
            old=self.tabs.widget(0);self.tabs.removeTab(0);old.deleteLater()
        self.controls={}
        preferred=['offline','music_servers','paths','qbt','providers','catalog','ai','lyrics']
        keys=preferred+[k for k in self.values if k not in preferred]
        advanced_tabs=QTabWidget()
        for key in keys:
            if key not in self.values:continue
            value=self.values[key]
            if key=='schema_version':continue
            body=QWidget();form=QFormLayout(body)
            self._walk(form,(key,),value)
            scroll=QScrollArea();scroll.setWidgetResizable(True);scroll.setWidget(body)
            if key in preferred:self.tabs.addTab(scroll,'运行模式' if key=='offline' else LABELS.get(key,key))
            else:advanced_tabs.addTab(scroll,LABELS.get(key,key))
        if advanced_tabs.count():self.tabs.addTab(advanced_tabs,'高级参数')
    def _walk(self,form,path,value):
        if isinstance(value,dict):
            group=QGroupBox(LABELS.get(path[-1],path[-1]));inner=QFormLayout(group)
            for key,v in value.items():self._walk(inner,path+(key,),v)
            form.addRow(group);return
        if path==('lyrics','write_mode'):
            control=QComboBox();control.addItem('旁挂歌词 / 发行封面','sidecar');control.addItem('格式感知嵌入标签','embedded');control.setCurrentIndex(max(0,control.findData(value)))
        elif isinstance(value,bool):control=QCheckBox();control.setChecked(value)
        elif isinstance(value,int):control=QSpinBox();control.setRange(-1,2_000_000_000);control.setValue(value)
        elif isinstance(value,float):control=QDoubleSpinBox();control.setRange(-1,2_000_000_000);control.setValue(value)
        elif isinstance(value,list):control=QPlainTextEdit('\n'.join(str(v) for v in value));control.setMaximumHeight(85);control.setToolTip('每行一项；保留原来的列表类型')
        else:control=QLineEdit(str(value))
        control.setObjectName('config.'+'.'.join(path));control.setToolTip('.'.join(path))
        if path[-1] in ('auth_mode','auth_header','permanent_delete','rules_verified','preserve_unknown','preserve_multivalue','auto_tmm','protocol'):
            control.setEnabled(False)
        self.controls[path]=(control,value)
        label=LABELS.get(path[-1],path[-1]);form.addRow(label,control)
        if isinstance(control,QLineEdit) and (path[0]=='paths' or (path[0]=='music_servers' and path[-1]=='library')):
            choose=QPushButton('选择路径…');choose.clicked.connect(lambda _,c=control,p=path:self._browse(c,p));form.addRow('',choose)
    def _browse(self,control,path):
        value=QFileDialog.getOpenFileName(self,'选择媒体工具')[0] if 'tools' in path else QFileDialog.getExistingDirectory(self,'选择目录',control.text())
        if value:control.setText(value)
    def collect(self):
        values=copy.deepcopy(self.values)
        for path,(control,original) in self.controls.items():
            if isinstance(control,QComboBox):value=control.currentData()
            elif isinstance(original,bool):value=control.isChecked()
            elif isinstance(original,(int,float)):value=control.value()
            elif isinstance(original,list):
                value=[v.strip() for v in control.toPlainText().splitlines() if v.strip()]
                if original and isinstance(original[0],int):value=[int(v) for v in value]
            else:value=control.text()
            node=values
            for key in path[:-1]:node=node[key]
            node[path[-1]]=value
        return values
    def _save(self):
        try:self.save_requested.emit(self.collect())
        except (ValueError,TypeError) as exc:QMessageBox.warning(self,'配置输入错误',str(exc))

class MainWindow(QMainWindow):
    def __init__(self,base,client=None,autostart=True,demo_profile=False,online_mode=False):
        super().__init__();self.base=Path(base).resolve();self.config_version=0;self.config_values={};self.last_seq=0;self.pending=0;self.jobs=set();self.callbacks={};self.task_callbacks={};self.task_fetching=set();self.plans={};self.last_results={};self.current_resources=[];self.status_cache={};self.row_cache={};self.status_detail_cache=None
        self.demo_profile=demo_profile;self.online_mode=online_mode;self.catalog_target_id=None;self.catalog_links={};self.catalog_resolved=set();self.catalog_query_serial=0
        self.library_offset=0;self.library_limit=200;self.library_total=0;self.library_page_serial=0;self.library_page_loaded=False
        if client is None:
            from musicops.worker.client import WorkerClient
            client=WorkerClient(self.base)
        self.client=client;self.pool=QThreadPool(self);self.pool.setMaxThreadCount(1)
        self.setWindowTitle(f'MusicOps {__version__} · 音乐库管理');self.resize(1280,860)
        central=QWidget();self.setCentralWidget(central);outer=QVBoxLayout(central)
        self.banner=QLabel(f'MusicOps {__version__} · 正在连接后台');self.banner.setObjectName('status_banner');self.banner.setWordWrap(True);outer.addWidget(self.banner)
        self.profile_label=QLabel('当前档案：'+str(self.base));self.profile_label.setObjectName('profile_path');self.profile_label.setTextInteractionFlags(Qt.TextInteractionFlag.TextSelectableByMouse);self.profile_label.setWordWrap(True);outer.addWidget(self.profile_label)
        horizontal=QSplitter();outer.addWidget(horizontal,1)
        self.nav=QListWidget();self.nav.addItems(PAGE_NAMES);self.nav.setFixedWidth(170);self.nav.setObjectName('navigation');horizontal.addWidget(self.nav)
        self.pages=QStackedWidget();horizontal.addWidget(self.pages);self.nav.currentRowChanged.connect(self.pages.setCurrentIndex)
        self.layouts={};self.tables={};self.details={};self.detail_groups={};self.result_summary={}
        for name in PAGE_NAMES:
            page=QWidget();layout=QVBoxLayout(page);heading=QLabel(f'<h2>{name}</h2>');heading.setMaximumHeight(48);layout.addWidget(heading);summary=QLabel();summary.setWordWrap(True);summary.setMaximumHeight(72);summary.setStyleSheet('color:#234566;padding:5px');layout.addWidget(summary);self.result_summary[name]=summary;self.pages.addWidget(page);self.layouts[name]=layout
        self._overview();self._playlists();self._catalog();self._search();self._downloads();self._library();self._repair();self._resources();self._ai();self._review();self._transactions();self._configuration();self._reports()
        self.nav.setCurrentRow(0)
        self.statusBar().showMessage('联网接口可从歌单、在线识别、歌词封面或配置页测试。首次启动不自动扫描音乐库。')
        self.timer=QTimer(self);self.timer.setInterval(2000);self.timer.timeout.connect(self.refresh)
        if autostart:QTimer.singleShot(0,lambda:self.request('attach',{},self._attached))
    def button(self,layout,text,callback,name=None):
        button=QPushButton(text);button.setObjectName(name or text);button.clicked.connect(callback);layout.addWidget(button);return button
    def controls(self,page):
        row=QHBoxLayout();self.layouts[page].addLayout(row);return row
    def table(self,page):
        table=QTableWidget();table.setSelectionBehavior(QTableWidget.SelectionBehavior.SelectRows);table.setEditTriggers(QTableWidget.EditTrigger.NoEditTriggers);table.setAlternatingRowColors(True);table.setObjectName('table.'+page);self.layouts[page].addWidget(table,1);self.tables[page]=table;return table
    def detail(self,page):
        text=QPlainTextEdit();text.setReadOnly(True);text.setMaximumHeight(220);text.setObjectName('detail.'+page);group=QGroupBox('详细证据与技术记录（点击展开）');group.setCheckable(True);group.setChecked(False);group.setMaximumHeight(40);layout=QVBoxLayout(group);layout.addWidget(text);text.setVisible(False);group.toggled.connect(text.setVisible);group.toggled.connect(lambda checked,g=group:g.setMaximumHeight(270 if checked else 40));self.layouts[page].addWidget(group);self.details[page]=text;self.detail_groups[page]=group
        if page in self.tables:self.tables[page].itemSelectionChanged.connect(lambda p=page:self.details[p].setPlainText(pretty(self.selected(p))))
        return text
    def show_rows(self,page,rows):
        table=self.tables[page];rows=rows or []
        if not isinstance(rows,list):rows=[rows]
        rows=[row if isinstance(row,dict) else {'值':row} for row in rows]
        original_rows=rows
        if page=='资源搜索':
            rows=[{**r,'format':r.get('media',{}).get('format'),'encoding':r.get('media',{}).get('encoding'),'edition':r.get('release',{}).get('edition'),'size_display':r.get('media',{}).get('size_display')} for r in rows]
        if self.row_cache.get(page)==rows:return
        previous=self.selected(page)
        self.row_cache[page]=copy.deepcopy(rows)
        table.setUpdatesEnabled(False);sorting=table.isSortingEnabled();table.setSortingEnabled(False)
        keys=[]
        for row in rows:
            for key in row:
                if key not in keys and key not in ('content_bytes','lyrics','payload','plan','operations','raw_tags','content_base64','config_snapshot','args','result'):keys.append(key)
        keys=[k for k in PREFERRED_COLUMNS if k in keys]+[k for k in keys if k not in PREFERRED_COLUMNS];keys=keys[:12];table.setColumnCount(len(keys));table.setHorizontalHeaderLabels([HEADERS.get(k,LABELS.get(k,k)) for k in keys]);table.setRowCount(len(rows))
        if page=='资源搜索':
            keys=[k for k in ('title','artists','source','format','encoding','edition','seeders','estimated_cost','size_display','download_status') if any(k in r for r in rows)]
            table.setColumnCount(len(keys));table.setHorizontalHeaderLabels([HEADERS.get(k,LABELS.get(k,k)) for k in keys])
        if page=='在线识别':
            keys=[k for k in ('source','title','artists','album','release_title','duration','score','confidence','match_state','match_reason','verification','requires_review','requires_manual_review','identity_confirmed','reason','candidate_id') if any(k in r for r in rows)]
            keys=keys[:12];table.setColumnCount(len(keys));table.setHorizontalHeaderLabels([HEADERS.get(k,LABELS.get(k,k)) for k in keys])
        for i,row in enumerate(rows):
            for j,key in enumerate(keys):
                value=row.get(key,'')
                if value is None:label='未确认'
                elif page=='在线识别' and key=='confidence' and type(value) in (int,float):label=f'{value*100:.0f}%'
                elif isinstance(value,bool):label='是' if value else '否'
                elif isinstance(value,list) and all(isinstance(v,(str,int)) for v in value):label='、'.join(str(v) for v in value)
                elif isinstance(value,(dict,list)):label='结构信息（见详情）'
                else:label=STATE_LABELS.get(str(value),str(value))
                item=QTableWidgetItem(label)
                if j==0:item.setData(Qt.ItemDataRole.UserRole,original_rows[i])
                table.setItem(i,j,item)
        table.horizontalHeader().setResizeContentsPrecision(100)
        table.resizeColumnsToContents()
        for column in range(table.columnCount()):table.setColumnWidth(column,min(290,table.columnWidth(column)))
        identity=next((k for k in ('target_id','candidate_id','task_id','file_id','tx_id','id') if previous.get(k)),None)
        if identity:
            for i,row in enumerate(rows):
                if row.get(identity)==previous[identity]:table.selectRow(i);break
        table.setSortingEnabled(sorting);table.setUpdatesEnabled(True)
    def selected(self,page):
        table=self.tables[page];row=table.currentRow()
        item=table.item(row,0) if row>=0 and table.columnCount() else None
        return (item.data(Qt.ItemDataRole.UserRole) or {}) if item else {}
    def request(self,command,payload=None,callback=None):
        job=IpcJob(self.client,command,payload or {});self.jobs.add(job);self.pending+=1
        self.callbacks[job]=callback
        job.signals.done.connect(lambda cmd,result,j=job:self._done(j,cmd,result));job.signals.failed.connect(lambda cmd,error,j=job:self._failed(j,cmd,error));self.pool.start(job)
        self.statusBar().showMessage(f'后台处理：{command}（界面仍可操作）')
        return job
    def _done(self,job,command,result):
        self.jobs.discard(job);self.pending=max(0,self.pending-1);callback=self.callbacks.pop(job,None)
        self.last_results[command]=result
        self.statusBar().showMessage(f'已收到：{command}')
        if isinstance(result,dict) and result.get('ok') is False:
            self.statusBar().showMessage(pretty(result));self.details['日志与报告'].setPlainText(pretty(result))
            return
        elif isinstance(result,dict) and result.get('accepted') and result.get('task_id'):
            self.task_callbacks[result['task_id']]=(command,callback)
            if callback:callback(result)
        elif callback:callback(result)
    def _failed(self,job,command,error):
        self.jobs.discard(job);self.pending=max(0,self.pending-1);self.callbacks.pop(job,None)
        message=error.get('message','请求失败') if isinstance(error,dict) else str(error)
        if isinstance(error,dict) and error.get('connection_error'):self.banner.setText('后台断连 · 任务状态保留，重新附着后查看恢复条件')
        self.last_results[command]={'ok':False,'error':message,'error_class':error.get('error_class','IPC_ERROR') if isinstance(error,dict) else 'IPC_ERROR'}
        cleanup=getattr(job,'failure_cleanup',None)
        if cleanup:cleanup()
        self.details['日志与报告'].setPlainText(message);self.detail_groups['日志与报告'].setChecked(True);self.statusBar().showMessage(message)
    def _attached(self,result):
        self.timer.start()
        if self.demo_profile:
            self.request('enable_demo',{},lambda r:(self._config_saved(r),self.request('demo',{},lambda d:(self.details['总览'].setPlainText(pretty(d)),self.request('get_status',{'compact':True},self._status)))))
        elif self.online_mode:self._enable_online()
        else:self.request('get_status',{'compact':True},self._status)
    def refresh(self):
        if not self.pending:self.request('get_status',{'compact':True},self._status)
    def _status(self,result):
        result=result.get('result',result) if isinstance(result,dict) else {}
        if 'config' not in result or 'tasks' not in result:
            if 'paused' in result:self.request('get_status',{'compact':True},self._status)
            else:self.statusBar().showMessage('后台状态响应不完整，请重新附着后台。')
            return
        config=result.get('config',{});self.config_version=config.get('version',self.config_version);self.config_values=config.get('values',self.config_values)
        paused=result.get('paused',False);mode='演示档案 · 尚未启用联网' if self.config_values.get('offline',False) else '正式联网模式'
        self.banner.setText(f"MusicOps {__version__} · {mode} · 配置 v{self.config_version} · 流水线{'暂停' if paused else '可派发'}")
        self.profile_label.setText('当前档案：'+str(self.base));self.enable_online_button.setEnabled(bool(self.config_values.get('offline',False)))
        self.status_cache=result
        if self.status_detail_cache!=result:
            self.details['总览'].setPlainText(pretty(result));self.status_detail_cache=copy.deepcopy(result)
        self.show_rows('下载与保种',result.get('tasks',[]))
        if not self.editor.values:
            self.editor.load(config)
            self.library_root.setText(self.config_values.get('paths',{}).get('library',''))
            self._secret_purpose_changed()
        txs=result.get('transactions',[])
        self.show_rows('事务与回滚',txs)
        reviews=result.get('reviews',[])
        self.show_rows('复核中心',reviews)
        matches=[row for row in reviews if row.get('main_state')]
        counts={state:sum(r['main_state']==state for r in matches) for state in ('verified_present','missing','ambiguous','version_mismatch','source_unavailable','snapshot_incomplete')}
        self.coverage_summary.setText(f"已存在且确证　{counts['verified_present']}\n明确缺口　{counts['missing']}\n待复核　{counts['ambiguous']+counts['version_mismatch']}\n来源或快照受阻　{counts['source_unavailable']+counts['snapshot_incomplete']}")
        task_counts=result.get('counts',{})
        if 'library_file_count' in result:self.library_count_label.setText(f"索引总计 {result['library_file_count']} 个文件；分页每次最多读取 {self.library_limit} 条。")
        self.task_summary.setText(f"进行中　{task_counts.get('running',0)}\n排队　{task_counts.get('queued',0)}\n已完成　{task_counts.get('completed',0)}\n失败 / 受阻　{task_counts.get('failed',0)+task_counts.get('blocked',0)}")
        self.result_summary['总览'].setText(f"当前库：{self.config_values.get('paths',{}).get('library','')}　｜　联网识别、歌单和资源查询可在相应页面直接测试。文件改动需查看并执行计划，Roon显示另行验收。")
        for task in result.get('tasks',[]):
            task_id=task.get('task_id')
            if task_id in self.task_callbacks and task.get('state') in ('completed','blocked','failed','cancelled','recovery_required'):
                if 'result' in task:self._task_received(task_id,task)
                elif task_id not in self.task_fetching:
                    self.task_fetching.add(task_id)
                    job=self.request('get_task',{'task_id':task_id},lambda response,t=task_id:self._task_received(t,response.get('task',{})))
                    job.failure_cleanup=lambda t=task_id:self.task_fetching.discard(t)
    def _task_received(self,task_id,task):
        self.task_fetching.discard(task_id)
        if task.get('task_id')!=task_id or task.get('state') not in ('completed','blocked','failed','cancelled','recovery_required'):return
        entry=self.task_callbacks.pop(task_id,None)
        if not entry:return
        command,callback=entry;result=task.get('result')
        final=(result or {}) if task.get('state')=='completed' else {**(result if isinstance(result,dict) else {}),'state':task.get('state'),'error':task.get('error'),'error_class':task.get('error_class')}
        self.last_results[command]=final
        if callback:callback(final)
    def result_to_page(self,page,result):
        result=result.get('result',result) if isinstance(result,dict) else result
        detail_result={k:v for k,v in result.items() if k!='targets'} if page=='歌单与补齐' and isinstance(result,dict) else result
        self.details[page].setPlainText(pretty(detail_result))
        if isinstance(result,dict):
            if result.get('accepted'):self.result_summary[page].setText('已加入后台任务队列。可继续操作；完成后此页面自动更新。')
            elif result.get('error'):self.result_summary[page].setText('处理受阻：'+str(result['error']));self.detail_groups[page].setChecked(True)
            else:self.result_summary[page].setText('结果已更新。选择一行查看身份、证据与理由。')
            if page=='AI 助手':
                self.ai_output.setPlainText(str(result.get('explanation',result.get('answer',result.get('error','')))))
                if 'proposed_actions' in result:self.show_rows(page,result['proposed_actions'])
                self.result_summary[page].setText('AI建议已生成；需要审核。' if not result.get('error') else 'AI请求未通过：'+str(result['error']))
            if page=='资源搜索' and 'source_outcomes' in result and result.get('read_only'):
                states=[]
                for site,outcome in result['source_outcomes'].items():states.append(site+'：'+STATE_LABELS.get(outcome.get('status'),outcome.get('status','未知'))+' '+str(outcome.get('candidate_count',0))+'项')
                self.result_summary[page].setText('；'.join(states)+'。当前为第一页候选，下载资格仍待确认。')
            if page=='歌单与补齐' and 'targets' in result:
                self.result_summary[page].setText(f"已解析 {len(result['targets'])} 个可见曲目。"+('完整快照已通过。' if result.get('complete') else '完整性仍有待对账项；此清单不驱动下载。' if result.get('read_only') else '已载入本机保存的曲目；未发网络请求。'))
            if page=='在线识别':self._catalog_result(result)
            if page=='音乐库与音质' and isinstance(result.get('files'),list):self._library_page_result(result)
            if page=='歌词与封面' and ('lyrics' in result or 'cover' in result):
                rows=[]
                for kind in ('lyrics','cover'):
                    rows.extend(result.get(kind,{}).get('candidates',[]))
                if result.get('cover_preview'):
                    for item in rows:
                        if item.get('kind')=='cover':item['path']=result['cover_preview']
                self.show_rows(page,rows)
            for key in ('targets','files','candidates','decisions','matches','tasks','groups','items','events','transactions','reviews','suggestions','snapshots','links','playlist_ids','valid_ids'):
                if key in result and isinstance(result[key],list):self.show_rows(page,result[key]);break
            if 'plan' in result:self._show_plan(page,result['plan'])
        elif isinstance(result,list):self.show_rows(page,result)
    def _show_plan(self,page,plan):
        self.plans[page]=plan;self.details[page].setPlainText('待执行计划（审核受影响文件、前置哈希、备份及目标路径）：\n'+pretty(plan))
        operations=plan.get('operations',[]);targets='；'.join(op.get('destination','') for op in operations)
        self.result_summary[page].setText(f"待审核：{len(operations)}项操作。目标：{targets}。备份位置：{self.config_values.get('paths',{}).get('backup','当前档案备份区')}。预算：{plan.get('budget','无新增令牌')}。请查看前后差异，再点击执行。")
        preview=plan.get('preview',{})
        if isinstance(preview,dict) and preview.get('fields'):self.show_rows(page,preview['fields'])
    def execute_plan(self,page):
        plan=self.plans.get(page)
        if not plan:self.details[page].setPlainText('请先生成并预览计划。');return
        reviewed=copy.deepcopy(plan);reviewed['reviewed']=True
        self.request('submit_plan',{'plan':reviewed},lambda r:self.result_to_page(page,r))
    def _overview(self):
        page='总览';row=self.controls(page)
        self.enable_online_button=self.button(row,'启用当前档案联网',self._enable_online,'enable_online');self.button(row,'打开独立演示档案',self._demo,'demo');self.button(row,'附着后台',lambda:self.request('attach',{},self._attached),'attach');self.button(row,'暂停流水线',lambda:self.request('pause_pipeline',{},self._status),'pause_pipeline');self.button(row,'恢复流水线',lambda:self.request('resume_pipeline',{},self._status),'resume_pipeline')
        self.layouts[page].addWidget(QLabel('文件层通过与Roon显示通过分别记录。暂停流水线让当前写事务到安全点，已有下载和做种继续。'))
        summaries=QHBoxLayout();self.layouts[page].addLayout(summaries);self.coverage_summary=QLabel('尚未创建演示数据');self.task_summary=QLabel('尚无任务')
        for title,label in [('曲目覆盖与复核',self.coverage_summary),('后台任务',self.task_summary)]:
            group=QGroupBox(title);layout=QVBoxLayout(group);layout.addWidget(label);summaries.addWidget(group)
        for label,target in [('补齐歌单','歌单与补齐'),('歌曲 / 专辑在线识别','在线识别'),('修复库信息','库体检与修复'),('音质对比去重','音乐库与音质')]:self.button(self.layouts[page],label,lambda _,p=target:self.nav.setCurrentRow(PAGE_NAMES.index(p)))
        self.detail(page);self.layouts[page].addStretch(1)
    def _enable_online(self):
        self.request('enable_online',{},lambda r:(self._config_saved(r),self.details['总览'].setPlainText(pretty(r)),self.request('get_status',{'compact':True},self._status)))
    def _demo(self):
        if self.config_values.get('offline',False):
            self.request('demo',{},lambda r:(self.details['总览'].setPlainText(pretty(r)),self.request('get_status',{'compact':True},self._status)));return
        demo_base=self.base/'demo-profile'
        arguments=['--base',str(demo_base),'--demo']
        command=([sys.executable] if getattr(sys,'frozen',False) else [sys.executable,'-m','musicops.gui.main'])+arguments
        try:
            subprocess.Popen(command,cwd=str(self.base),creationflags=getattr(subprocess,'CREATE_NO_WINDOW',0))
            self.statusBar().showMessage('已打开独立演示档案：'+str(demo_base))
        except OSError:self.statusBar().showMessage('演示窗口启动失败；请查看当前档案日志。')
    def _playlists(self):
        page='歌单与补齐';self.playlist_input=QPlainTextEdit();self.playlist_input.setPlaceholderText('粘贴多个网易云歌单链接、分享文本或ID；解析只在本机进行');self.playlist_input.setMaximumHeight(110);self.playlist_input.setObjectName('playlist_input');self.layouts[page].addWidget(self.playlist_input)
        row=self.controls(page);self.button(row,'本地解析歌单',lambda:self.request('parse_playlist',{'text':self.playlist_input.toPlainText()},lambda r:self.result_to_page(page,r)),'parse_playlist');self.button(row,'保存歌单到配置',lambda:self.request('add_playlists',{'text':self.playlist_input.toPlainText()},self._config_saved),'add_playlists');self.button(row,'刷新歌单快照',lambda:self.request('refresh_playlist',{},lambda r:self.result_to_page(page,r)),'refresh_playlist');self.button(row,'逐曲比对音乐库',lambda:self.request('compare',{},lambda r:self.result_to_page(page,r)),'compare');self.button(row,'搜索选中曲目',self._search_selected,'search_selected')
        row2=self.controls(page);self.button(row2,'查看已保存曲目（不联网）',lambda:self.request('get_playlist_targets',{},lambda r:self.result_to_page(page,r)),'get_playlist_targets');self.button(row2,'查询选中曲目的歌词 / 封面',self._playlist_resources,'playlist_resources');self.button(row2,'在线识别选中曲目 / 专辑',lambda:self._catalog_from_row('歌单与补齐'),'playlist_identify')
        self.table(page);self.detail(page)
    def _search_selected(self):
        selected=self.selected('歌单与补齐');title=selected.get('raw_title') or selected.get('title')
        if not title:self.statusBar().showMessage('请先查看已保存曲目并选择一首歌。');return
        artists=' '.join(selected.get('artists',[]));self.query.setText(str(title)+(' '+artists if artists else ''));self.nav.setCurrentRow(PAGE_NAMES.index('资源搜索'));self._do_search()
    def _playlist_resources(self):
        selected=self.selected('歌单与补齐')
        if not (selected.get('raw_title') or selected.get('title')):self.statusBar().showMessage('请先查看已保存曲目并选择一首歌。');return
        self.resource_title.setText(str(selected.get('raw_title',selected.get('title',''))));self.resource_artist.setText(' / '.join(selected.get('artists',[])));self.resource_album.setText(str(selected.get('album','')));self.duration.setValue(selected.get('duration') or 0)
        self.nav.setCurrentRow(PAGE_NAMES.index('歌词与封面'));self._fetch_resources()
    def _catalog(self):
        page='在线识别';form=QFormLayout();self.layouts[page].addLayout(form)
        self.catalog_title=QLineEdit();self.catalog_title.setObjectName('catalog_title');self.catalog_title.setPlaceholderText('曲名；识别专辑可留空并填写专辑')
        self.catalog_artist=QLineEdit();self.catalog_artist.setObjectName('catalog_artist');self.catalog_artist.setPlaceholderText('多个艺人以 / 分隔')
        self.catalog_album=QLineEdit();self.catalog_album.setObjectName('catalog_album');self.catalog_album.setPlaceholderText('已知专辑；缺失时搜索发行候选')
        self.catalog_duration=QDoubleSpinBox();self.catalog_duration.setObjectName('catalog_duration');self.catalog_duration.setRange(0,100000);self.catalog_duration.setSuffix(' 秒')
        form.addRow('歌曲',self.catalog_title);form.addRow('艺人',self.catalog_artist);form.addRow('专辑',self.catalog_album);form.addRow('时长（0为未知）',self.catalog_duration)
        self.catalog_target=QLabel('当前为独立检索；从歌单选择曲目可将候选确认到本地目标清单。');self.catalog_target.setWordWrap(True);self.catalog_target.setObjectName('catalog_target');self.layouts[page].addWidget(self.catalog_target)
        row=self.controls(page);self.button(row,'在线检索歌曲 / 专辑',self._identify_metadata,'identify_metadata');self.button(row,'读取选中候选的完整发行证据',self._resolve_metadata,'resolve_metadata');self.button(row,'确认到目标清单 / 文件提案',self._accept_metadata,'accept_metadata')
        self.button(row,'独立检索（解除曲目绑定）',self._catalog_unbind,'catalog_unbind')
        self.layouts[page].addWidget(QLabel('MusicBrainz可直接检索，Discogs需在配置页保存Token。AllMusic、Rate Your Music、Album of the Year、VGMdb通过官网核对。请核对录音、发行和版本；确认仅修改本地目标清单，不改音乐文件。'))
        links=self.controls(page);self.catalog_link_buttons={}
        for source,label in [('musicbrainz','MusicBrainz'),('discogs','Discogs'),('allmusic','AllMusic'),('rateyourmusic','Rate Your Music'),('albumoftheyear','Album of the Year'),('vgmdb','VGMdb')]:
            button=self.button(links,label,lambda _,s=source:self._open_catalog_link(s),'catalog_link_'+source);button.setEnabled(False);self.catalog_link_buttons[source]=button
        self.table(page);self.detail(page)
    def _catalog_unbind(self):
        self.catalog_target_id=None;self.catalog_query_serial+=1;self.catalog_resolved.clear();self.catalog_target.setText('当前为独立检索；候选可供核对，音乐文件保持原样。')
    def _catalog_from_row(self,page):
        item=self.selected(page)
        if not item:self.statusBar().showMessage('请先选择一首曲目或已索引文件。');return
        self._load_catalog_query(item);self.nav.setCurrentRow(PAGE_NAMES.index('在线识别'));self._identify_metadata()
    def _load_catalog_query(self,item):
        title=item.get('raw_title') or item.get('title') or '';artists=item.get('artists',item.get('artist',[]));album=item.get('album','')
        if isinstance(title,list):title=title[0] if title else ''
        if isinstance(album,list):album=album[0] if album else ''
        if isinstance(artists,str):artists=[artists]
        self.catalog_title.setText(str(title));self.catalog_artist.setText(' / '.join(str(a) for a in artists));self.catalog_album.setText(str(album));self.catalog_duration.setValue(item.get('duration') or 0)
        self.catalog_target_id=item.get('target_id') or item.get('file_id');self.catalog_query_serial+=1;self.catalog_resolved.clear()
        self.catalog_target.setText('目标文件：'+str(self.catalog_target_id)+'；确认仅保存元数据提案，原始标签保持原样。' if item.get('file_id') else '目标曲目：'+str(self.catalog_target_id)+'；确认候选仅修正此目标清单。' if self.catalog_target_id else '当前为独立检索；请核对发行信息。')
    def _identify_metadata(self):
        title=self.catalog_title.text().strip();album=self.catalog_album.text().strip();artists=[a.strip() for a in self.catalog_artist.text().split(' / ') if a.strip()]
        if not title and not album:self.statusBar().showMessage('请填写曲名或专辑名称后检索。');return
        self.catalog_query_serial+=1;serial=self.catalog_query_serial;self.catalog_resolved.clear();self.catalog_links={};self.show_rows('在线识别',[])
        for button in self.catalog_link_buttons.values():button.setEnabled(False)
        payload={'title':title,'artists':artists,'album':album,'duration':self.catalog_duration.value() or None}
        if self.catalog_target_id:payload['target_id']=self.catalog_target_id
        self.request('identify_metadata',payload,lambda r:self._catalog_search_received(serial,r))
    def _catalog_search_received(self,serial,result):
        if serial==self.catalog_query_serial:self.result_to_page('在线识别',result)
    def _resolve_metadata(self):
        item=self.selected('在线识别');candidate_id=item.get('candidate_id')
        if not candidate_id:self.statusBar().showMessage('请先选择候选发行。');return
        serial=self.catalog_query_serial
        self.request('resolve_metadata',{'candidate_id':candidate_id},lambda r:self._metadata_resolved(candidate_id,r) if serial==self.catalog_query_serial else None)
    def _metadata_resolved(self,candidate_id,result):
        result=result.get('result',result) if isinstance(result,dict) else {}
        if result.get('accepted') or result.get('error'):
            self.result_to_page('在线识别',result);return
        candidate=result.get('candidate')
        if not candidate and isinstance(result.get('candidates'),list):candidate=next((c for c in result['candidates'] if c.get('candidate_id')==candidate_id),None)
        if candidate:
            self.catalog_resolved.add(candidate.get('candidate_id',candidate_id))
            rows=[candidate if row.get('candidate_id')==candidate_id else row for row in self.row_cache.get('在线识别',[])]
            self.show_rows('在线识别',rows)
        if result.get('lookup_links'):self._catalog_result(result)
        self.details['在线识别'].setPlainText(pretty(result));self.detail_groups['在线识别'].setChecked(True)
        self.result_summary['在线识别'].setText('候选详情已读取，请核对曲名、艺人、专辑、录音及发行版本，再确认到目标清单。' if candidate else '未取得候选发行详情；请查看证据与来源状态。')
    def _accept_metadata(self):
        item=self.selected('在线识别');candidate_id=item.get('candidate_id')
        if not candidate_id:self.statusBar().showMessage('请先选择候选并读取完整发行证据。');return
        if candidate_id not in self.catalog_resolved:self.statusBar().showMessage('请先读取选中候选的完整发行证据并核对。');return
        if not self.catalog_target_id:self.statusBar().showMessage('当前为独立检索；请从歌单或音乐库选择要确认的目标。');return
        self.request('accept_metadata',{'candidate_id':candidate_id,'target_id':self.catalog_target_id},self._metadata_accepted)
    def _metadata_accepted(self,result):
        self.result_to_page('在线识别',result)
        if isinstance(result,dict) and not result.get('accepted') and not result.get('error') and result.get('state') not in ('blocked','failed','cancelled','recovery_required') and result.get('status','OK')=='OK':
            if result.get('file') or result.get('metadata_only'):
                self.result_summary['在线识别'].setText('已记录文件元数据提案和来源证据；音乐文件与原始标签保持原样。');self._load_library_page(self.library_offset)
            else:
                self.result_summary['在线识别'].setText('已记录候选确认与证据；本地目标清单已更新，音乐文件保持原样。');self.request('get_playlist_targets',{},lambda r:self.result_to_page('歌单与补齐',r))
    def _catalog_result(self,result):
        for link in result.get('lookup_links',[]):
            if not isinstance(link,dict):continue
            source=str(link.get('source','')).lower().replace(' ','').replace('_','').replace('-','')
            aliases={'rateyourmusic':'rateyourmusic','rym':'rateyourmusic','albumoftheyear':'albumoftheyear','aoty':'albumoftheyear'};source=aliases.get(source,source)
            if source in self.catalog_link_buttons:
                self.catalog_links[source]=link.get('url','');self.catalog_link_buttons[source].setEnabled(self._catalog_url_allowed(link.get('url','')))
        if result.get('accepted'):return
        outcomes=result.get('sources',result.get('source_outcomes',[]));parts=[]
        if isinstance(outcomes,dict):outcomes=[{'source':source,**outcome} for source,outcome in outcomes.items() if isinstance(outcome,dict)]
        for outcome in outcomes:
            if isinstance(outcome,dict):parts.append(str(outcome.get('source',outcome.get('name','')))+'：'+STATE_LABELS.get(outcome.get('status',''),str(outcome.get('status','待复核'))))
        if not result.get('error'):
            summary=('；'.join(parts)+'。' if parts else '')+f"共 {len(result.get('candidates',[]))} 个候选；排序分数供参考，请读取完整发行证据后确认。"
            if result.get('message'):summary=STATE_LABELS.get(result.get('status',''),result.get('status',''))+'：'+str(result['message'])+('；'+'；'.join(parts) if parts else '')
            self.result_summary['在线识别'].setText(summary)
    @staticmethod
    def _catalog_url_allowed(url):
        try:
            parsed=urlsplit(url);hosts={'musicbrainz.org','www.discogs.com','discogs.com','www.allmusic.com','allmusic.com','rateyourmusic.com','www.rateyourmusic.com','www.albumoftheyear.org','albumoftheyear.org','vgmdb.net','www.vgmdb.net'}
            return parsed.scheme=='https' and parsed.hostname in hosts and parsed.port in (None,443) and not parsed.username and not parsed.password
        except (TypeError,ValueError):return False
    def _open_catalog_link(self,source):
        url=self.catalog_links.get(source,'')
        if self._catalog_url_allowed(url):QDesktopServices.openUrl(QUrl(url))
        else:self.statusBar().showMessage('请先检索以取得该来源的官方核对链接。')
    def _search(self):
        page='资源搜索';row=self.controls(page);self.query=QLineEdit();self.query.setPlaceholderText('单曲 / 专辑 / 艺人');self.query.setObjectName('search_query');row.addWidget(self.query);self.button(row,'四来源检索',self._do_search,'search');self.button(row,'选择唯一候选并预览',self._candidate_plan,'candidate_plan');self.button(row,'运行已审核补齐计划',lambda:self.execute_plan(page),'execute_fill')
        self.table(page);self.detail(page)
    def _do_search(self):
        query=self.query.text().strip()
        if not query:self.statusBar().showMessage('请填写曲名、专辑或艺人后搜索。');return
        self.request('search',{'query':query},lambda r:self.result_to_page('资源搜索',r))
    def _candidate_plan(self):
        selected=self.selected('资源搜索')
        if not selected:self.details['资源搜索'].setPlainText('请选择一个候选，查看录音/发行版本、免费证据与预算。');return
        self.request('prepare_fill',{'candidate':selected},lambda r:self.result_to_page('资源搜索',r))
    def _downloads(self):
        page='下载与保种';row=self.controls(page);self.button(row,'刷新任务',lambda:self.request('list_tasks',{},lambda r:self.result_to_page(page,r)),'list_tasks');self.button(row,'取消选中流水线任务',lambda:self.request('cancel_task',{'task_id':self.selected(page).get('task_id',self.selected(page).get('id'))},lambda r:self.result_to_page(page,r)),'cancel_task');self.button(row,'停止派发新任务',lambda:self.request('pause_pipeline',{},self._status));self.button(row,'恢复派发',lambda:self.request('resume_pipeline',{},self._status))
        self.layouts[page].addWidget(QLabel('取消不代表回滚。令牌不可撤销；下载和保种责任独立记录。只显示本项目任务。'))
        self.table(page);self.detail(page)
    def _library(self):
        page='音乐库与音质';row=self.controls(page);self.library_root=QLineEdit(str(self.base/'library'));self.library_root.setObjectName('library_root');row.addWidget(self.library_root);self.button(row,'选择目录',lambda:self._select_directory(self.library_root));self.button(row,'轻量建立索引（只读）',self._index_library,'index_library')
        self.layouts[page].addWidget(QLabel('轻量索引读取文件列表及标签，不进行全库解码；选中文件可用在线来源核对歌曲与专辑。'))
        row=self.controls(page);self.library_query=QLineEdit();self.library_query.setObjectName('library_query');self.library_query.setPlaceholderText('搜索已有索引：曲名、艺人、专辑或路径');self.library_query.returnPressed.connect(lambda:self._load_library_page(0));row.addWidget(self.library_query,1)
        self.button(row,'搜索 / 查看已有索引',lambda:self._load_library_page(0),'list_library_files')
        self.library_previous=self.button(row,'上一页',lambda:self._load_library_page(max(0,self.library_offset-self.library_limit)),'library_previous');self.library_previous.setEnabled(False)
        self.library_next=self.button(row,'下一页',lambda:self._load_library_page(self.library_offset+self.library_limit),'library_next');self.library_next.setEnabled(False)
        self.library_count_label=QLabel('索引尚未载入；可分页查看已保存索引。');self.library_count_label.setObjectName('library_file_count');self.layouts[page].addWidget(self.library_count_label)
        self.library_page_label=QLabel('每页最多200个文件。');self.library_page_label.setObjectName('library_page');self.layouts[page].addWidget(self.library_page_label)
        row=self.controls(page);self.button(row,'生成音质 / 去重建议',lambda:self.request('prepare_dedup',{},lambda r:self.result_to_page(page,r)),'prepare_dedup');self.button(row,'选择建议生成隔离计划',lambda:self.request('prepare_dedup',{'selected_file_id':self.selected(page).get('quarantine_file_id')},lambda r:self.result_to_page(page,r)),'select_dedup');self.button(row,'审核后执行隔离计划',lambda:self.execute_plan(page),'execute_dedup');self.button(row,'修复选中文件',self._repair_selected,'repair_selected');self.button(row,'在线识别选中文件',lambda:self._catalog_from_row(page),'library_identify')
        row=self.controls(page);self.button(row,'完整验证选中文件（只读）',self._validate_selected_file,'validate_selected_file');row.addWidget(QLabel('仅检查选中的文件；不自动解码整个音乐库。'),1)
        self.table(page);self.detail(page)
    def _index_library(self):
        self.library_query.clear();self.library_page_serial+=1;serial=self.library_page_serial
        self.request('index_library',{'root':self.library_root.text(),'lightweight':True},lambda r:self.result_to_page('音乐库与音质',r) if serial==self.library_page_serial else None)
    def _load_library_page(self,offset=0):
        self.library_page_serial+=1;serial=self.library_page_serial
        self.request('list_library_files',{'query':self.library_query.text().strip(),'offset':offset,'limit':self.library_limit},lambda r:self.result_to_page('音乐库与音质',r) if serial==self.library_page_serial else None)
    def _library_page_result(self,result):
        rows=result.get('files',[]);self.library_offset=result.get('offset',0);self.library_limit=result.get('limit',200);self.library_total=result.get('total',result.get('count',len(rows)));self.library_page_loaded=True
        self.library_previous.setEnabled(self.library_offset>0);self.library_next.setEnabled(self.library_offset+len(rows)<self.library_total and bool(rows))
        start=self.library_offset+1 if rows else 0;end=self.library_offset+len(rows)
        self.library_page_label.setText(f'当前显示 {start}–{end} / {self.library_total} 个文件；每页最多 {self.library_limit} 个。')
        self.result_summary['音乐库与音质'].setText(f'已载入 {len(rows)} 个文件；可搜索或翻页选择任意已索引文件。'+('当前为标签和文件信息，完整音质仍需逐文件验证。' if result.get('truncated') or result.get('lightweight') else ''))
    def _validate_selected_file(self):
        file_id=self.selected('音乐库与音质').get('file_id')
        if not file_id:self.statusBar().showMessage('请先选择一个已索引文件；完整验证仅处理此文件。');return
        self.request('validate_selected_file',{'file_id':file_id},self._selected_file_validated)
    def _selected_file_validated(self,result):
        self.result_to_page('音乐库与音质',result)
        if isinstance(result,dict) and isinstance(result.get('file'),dict):
            file=result['file'];self.show_rows('音乐库与音质',[file if row.get('file_id')==file.get('file_id') else row for row in self.row_cache.get('音乐库与音质',[])])
            self.result_summary['音乐库与音质'].setText('选中文件验证完成；结果和解码证据见详情，音乐文件保持原样。')
    def _select_directory(self,control):
        chosen=QFileDialog.getExistingDirectory(self,'选择获准目录',control.text())
        if chosen:control.setText(chosen)
    def _repair_selected(self):
        selected=self.selected('音乐库与音质');self.repair_selected_metadata=selected;self.repair_path.setText(str(selected.get('path','')));self.resource_path.setText(self.repair_path.text());self.nav.setCurrentRow(PAGE_NAMES.index('库体检与修复'))
    def _repair(self):
        page='库体检与修复';self.repair_selected_metadata={};row=self.controls(page);self.repair_path=QLineEdit();self.repair_path.setObjectName('repair_path');self.repair_path.setPlaceholderText('选择已索引的音频文件，并先预览修改内容');row.addWidget(self.repair_path);self.button(row,'选择文件',lambda:self._select_audio(self.repair_path))
        self.layouts[page].addWidget(QLabel('逐字段选择新值与锁定。多值字段每行一项；保留未选择及未知标签。先生成前后预览，再应用到独立副本。'))
        self.field_controls={};fields=QWidget();form=QFormLayout(fields)
        for field,label in [('title','标题'),('artist','艺人'),('album','专辑'),('albumartist','专辑艺人'),('tracknumber','轨号'),('discnumber','碟号'),('date','日期'),('composer','作曲者'),('genre','流派')]:
            row=QHBoxLayout();selected=QCheckBox('改动');value=QLineEdit();value.setPlaceholderText('多个值以 ; 分隔');locked=QCheckBox('锁定');value.setObjectName('repair.'+field);selected.setObjectName('repair.select.'+field);locked.setObjectName('repair.lock.'+field);row.addWidget(selected);row.addWidget(value,1);row.addWidget(locked);form.addRow(label,row);self.field_controls[field]=(selected,value,locked)
        self.layouts[page].addWidget(fields);row=self.controls(page);self.button(row,'生成字段修复预览',self._prepare_repair,'prepare_repair');self.button(row,'执行已审核修复',lambda:self.execute_plan(page),'execute_repair');self.button(row,'转到歌词封面',lambda:(self.resource_path.setText(self.repair_path.text()),self.nav.setCurrentRow(PAGE_NAMES.index('歌词与封面'))));self.button(row,'在线核对歌曲 / 专辑',self._catalog_from_repair,'repair_identify')
        self.table(page);self.detail(page)
    def _select_audio(self,control):
        chosen=QFileDialog.getOpenFileName(self,'选择音频文件',self.library_root.text(),'音频 (*.flac *.wav *.mp3 *.m4a *.ogg *.aiff *.dsf *.ape);;所有文件 (*)')[0]
        if chosen:control.setText(chosen)
    def _catalog_from_repair(self):
        item=dict(self.repair_selected_metadata) if self.repair_selected_metadata.get('path')==self.repair_path.text() else {}
        for field,key in [('title','title'),('artist','artists'),('album','album')]:
            value=self.field_controls[field][1].text().strip()
            if value:item[key]=[v.strip() for v in value.split(';') if v.strip()] if field=='artist' else value
        if not (item.get('title') or item.get('raw_title') or item.get('album')):
            self.statusBar().showMessage('请先从音乐库选择已索引文件，或填写曲名、艺人和专辑后核对。');return
        self._load_catalog_query(item);self.nav.setCurrentRow(PAGE_NAMES.index('在线识别'));self._identify_metadata()
    def _prepare_repair(self):
        changes={field:[x.strip() for x in value.text().split(';') if x.strip()] for field,(selected,value,lock) in self.field_controls.items() if selected.isChecked()};locks=[field for field,(_,_,lock) in self.field_controls.items() if lock.isChecked()]
        self.request('prepare_repair',{'path':self.repair_path.text(),'changes':changes,'locks':locks},lambda r:self.result_to_page('库体检与修复',r))
    def _resources(self):
        page='歌词与封面';form=QFormLayout();self.layouts[page].addLayout(form);self.resource_path=QLineEdit();self.resource_path.setObjectName('resource_path');self.resource_title=QLineEdit();self.resource_artist=QLineEdit();self.resource_album=QLineEdit();self.duration=QDoubleSpinBox();self.duration.setRange(0,100000);self.duration.setSuffix(' 秒');form.addRow('对应音频',self.resource_path);form.addRow('标题',self.resource_title);form.addRow('艺人',self.resource_artist);form.addRow('专辑 / 发行',self.resource_album);form.addRow('已知时长（0为未知）',self.duration)
        row=self.controls(page);self.button(row,'检索歌词与封面候选',self._fetch_resources,'fetch_resources');self.resource_lock=QCheckBox('锁定现有资源');row.addWidget(self.resource_lock);self.button(row,'预览选中候选',self._preview_resource,'preview_resource')
        self.button(row,'验证私有接口（只读）',lambda:self.request('lyrics_preflight',self._resource_query(),lambda r:self.result_to_page(page,r)),'lyrics_preflight')
        self.table(page)
        split=QSplitter();self.lrc_editor=QPlainTextEdit();self.lrc_editor.setObjectName('lrc_editor');self.cover_preview=QLabel('选择封面候选以预览');self.cover_preview.setAlignment(Qt.AlignmentFlag.AlignCenter);split.addWidget(self.lrc_editor);split.addWidget(self.cover_preview);self.layouts[page].addWidget(split)
        row=self.controls(page);self.offset=QSpinBox();self.offset.setRange(-600000,600000);self.offset.setSuffix(' 毫秒');row.addWidget(QLabel('时间偏移'));row.addWidget(self.offset);self.button(row,'预览偏移',self._shift,'offset_preview');self.button(row,'生成资源修复计划',self._prepare_resource,'prepare_resource');self.button(row,'执行已审核资源计划',lambda:self.execute_plan(page),'execute_resource');self.detail(page)
    def _fetch_resources(self):
        payload=self._resource_query()
        self.request('resources',payload,lambda r:self.result_to_page('歌词与封面',r))
    def _resource_query(self):
        artist=self.resource_artist.text();artists=[v.strip() for v in artist.split(' / ') if v.strip()]
        return {'title':self.resource_title.text(),'artist':artists if len(artists)>1 else artist,'album':self.resource_album.text(),'duration':self.duration.value() or None}
    def _preview_resource(self):
        item=self.selected('歌词与封面');self.details['歌词与封面'].setPlainText(pretty(item));self.lrc_editor.setPlainText(item.get('lyrics',''));data=item.get('content_bytes');self.cover_preview.clear();self.cover_preview.setText('当前候选没有可预览封面')
        if item.get('content_base64'):
            try:data=base64.b64decode(item['content_base64'],validate=True)
            except ValueError:data=None
        if item.get('path'):
            pix=QPixmap(item['path'])
            if not pix.isNull():self.cover_preview.setPixmap(pix.scaled(320,240,Qt.AspectRatioMode.KeepAspectRatio));data=None
        if isinstance(data,dict):data=None
        if data:
            pix=QPixmap();pix.loadFromData(data)
            if not pix.isNull():self.cover_preview.setPixmap(pix.scaled(320,240,Qt.AspectRatioMode.KeepAspectRatio))
    def _shift(self):
        try:
            shifted=shift_lrc(self.lrc_editor.toPlainText(),self.offset.value());self.lrc_editor.setPlainText(shifted);self.offset.setValue(0);self.details['歌词与封面'].setPlainText(pretty(validate_lrc(shifted,self.duration.value() or None)))
        except ValueError as exc:self.details['歌词与封面'].setPlainText(str(exc))
    def _prepare_resource(self):
        item=self.selected('歌词与封面')
        if not item:self.statusBar().showMessage('请选择资源候选并查看身份与内容校验结果。');return
        if item.get('identity_state')=='mismatch':self.statusBar().showMessage('候选标题、艺人、专辑或版本不符，不能生成写入计划。');return
        self.request('prepare_resource',{'path':self.resource_path.text(),'kind':item.get('kind','lyrics'),'candidate':item,'lyrics':self.lrc_editor.toPlainText(),'duration':self.duration.value() or None,'locked':self.resource_lock.isChecked(),'approved':True},lambda r:self.result_to_page('歌词与封面',r))
    def _ai(self):
        page='AI 助手';self.ai_input=QPlainTextEdit();self.ai_input.setPlaceholderText('用自然语言描述：搜索、补齐、修复、去重或回滚。AI只生成建议/计划，动作仍由软件执行层审核。');self.layouts[page].addWidget(self.ai_input)
        self.ai_input.setMaximumHeight(120);self.ai_output=QPlainTextEdit();self.ai_output.setReadOnly(True);self.ai_output.setPlaceholderText('AI分析说明将显示在这里；建议操作显示在下方。');self.ai_output.setMaximumHeight(160);self.layouts[page].addWidget(self.ai_output)
        row=self.controls(page);self.ai_ask=QLineEdit();self.ai_ask.setPlaceholderText('补充问题（可选）');row.addWidget(self.ai_ask,1)
        row2=self.controls(page);self.button(row2,'AI分析当前状态',self._analyze_ai,'ai_analyze');self.button(row2,'生成可审核计划',lambda:self.request('ai_plan',{'message':self.ai_input.toPlainText()},lambda r:self.result_to_page(page,r)),'ai_plan')
        self.button(row2,'测试AI连接（消耗少量token）',lambda:self.request('ai_test',{},lambda r:self.result_to_page(page,r)),'ai_test')
        self.layouts[page].addWidget(QLabel('AI不会直接改文件、下载、消费令牌或停种。所有建议必须先生成计划并在执行层复核。'))
        self.table(page);self.detail(page)
    def _analyze_ai(self):
        files=self.status_cache.get('files',[]);reviews=self.status_cache.get('reviews',[])
        file_keys=('file_id','path','title','artists','album','validation_state','duration','format')
        review_keys=('target_id','file_id','main_state','reason','manual_choice','confidence')
        context={'files':[{k:r[k] for k in file_keys if k in r} for r in files[:50]],'reviews':[{k:r[k] for k in review_keys if k in r} for r in reviews[:50]],'totals':{'files':len(files),'reviews':len(reviews)},'truncated':{'files':len(files)>50,'reviews':len(reviews)>50}}
        self.request('ai_analyze',{'context':context,'instructions':self.ai_ask.text()},lambda r:self.result_to_page('AI 助手',r))
    def _review(self):
        page='复核中心';self.review_reason=QLineEdit();self.review_reason.setPlaceholderText('记录证据与确认理由，供后续追溯');self.layouts[page].addWidget(self.review_reason);row=self.controls(page)
        for label,choice in [('确认候选','accept'),('拒绝候选','reject'),('保留多版本','keep_versions'),('锁定字段','lock'),('解除锁定','unlock')]:self.button(row,label,lambda _,c=choice:self.request('review_decision',{'identity':self.selected(page).get('target_id',self.selected(page).get('file_id',self.selected(page).get('identity',''))),'choice':c,'reason':self.review_reason.text(),'fields':[] if c=='unlock' else self.config_values.get('metadata',{}).get('locked_fields',[])},lambda r:self.result_to_page(page,r)))
        self.table(page);self.detail(page)
    def _transactions(self):
        page='事务与回滚';row=self.controls(page);self.tx_id=QLineEdit();self.tx_id.setPlaceholderText('事务ID；也可从表格选择');self.tx_id.setObjectName('tx_id');row.addWidget(self.tx_id);self.button(row,'查看事务与恢复条件',lambda:self.request('get_status',{'compact':True},lambda r:(self.result_to_page(page,r),self._status(r))),'show_transactions');self.button(row,'恢复指定事务',lambda:self.request('request_rollback',{'tx_id':self.tx_id.text() or self.selected(page).get('tx_id',self.selected(page).get('id'))},lambda r:self.result_to_page(page,r)),'request_rollback')
        self.layouts[page].addWidget(QLabel('回滚前复核目标哈希；外部改动进入CONFLICT。恢复原文件及伴随资源，保种源继续保持。'))
        self.table(page);self.detail(page)
    def _configuration(self):
        page='配置';self.editor=ConfigEditor();self.editor.save_requested.connect(lambda values:self.request('update_config',{'values':values,'expected_version':self.editor.version_number},self._config_saved));self.layouts[page].addWidget(self.editor,1)
        row=self.controls(page);self.button(row,'重新载入',lambda:self.request('get_status',{'compact':True},lambda r:self.editor.load(r.get('result',r).get('config',{}))));self.button(row,'脱敏导出配置',self._export_config,'export_config');self.button(row,'导入配置',self._import_config,'import_config')
        row2=self.controls(page);self.cookie_site=QComboBox()
        for name in ('dicmusic','orpheus','open.cd'):self.cookie_site.addItem(name,name)
        row2.addWidget(self.cookie_site);self.button(row2,'导入Cookie文件',self._import_pt_cookie,'import_pt_cookies');self.button(row2,'验证所选PT登录（只读）',lambda:self.request('pt_preflight',{'site':self.cookie_site.currentData()},lambda r:self.result_to_page('日志与报告',r)),'pt_preflight')
        row3=self.controls(page);self.button(row3,'检查已保存配置的Roon Core / Storage',lambda:self.request('roon_preflight',{},self._roon_checked),'roon_preflight');self.roon_connection=QLabel('Roon Core / Storage尚未检查');self.roon_connection.setObjectName('roon_connection');self.roon_connection.setWordWrap(True);row3.addWidget(self.roon_connection,1)
        secret=QGroupBox('Windows当前账户保护的登录信息 / 密钥');form=QFormLayout(secret)
        self.secret_purpose=QComboBox();self.secret_purpose.addItem('私有歌词接口原始鉴权','lyrics');self.secret_purpose.addItem('AI接口密钥','ai');self.secret_purpose.addItem('qBittorrent用户名和密码','qbt');self.secret_purpose.addItem('Discogs个人访问Token','discogs');self.secret_purpose.addItem('Navidrome密码','navidrome');self.secret_purpose.addItem('自定义凭据','custom')
        self.secret_ref=QLineEdit('vault://lyrics/private/authorization');self.secret_host=QLineEdit('localhost');self.secret_host.setPlaceholderText('明确授权域名 / IP，以逗号分隔');self.secret_username=QLineEdit();self.secret_value=QLineEdit();self.secret_value.setEchoMode(QLineEdit.EchoMode.Password)
        form.addRow('凭据用途',self.secret_purpose);form.addRow('用户名（仅qBittorrent）',self.secret_username);form.addRow('密码 / 密钥 / 原始鉴权',self.secret_value);form.addRow('凭据引用（自动填入）',self.secret_ref);form.addRow('绑定域名（自动填入）',self.secret_host)
        self.secret_username.setEnabled(False);self.secret_purpose.currentIndexChanged.connect(self._secret_purpose_changed)
        self.button(form,'保存登录信息 / 密钥',self._save_secret,'set_secret');self.button(form,'测试qBittorrent连接（只读）',lambda:self.request('qbt_preflight',{},lambda r:self.result_to_page('日志与报告',r)),'qbt_preflight');self.layouts[page].addWidget(secret)
        self.button(form,'导入歌词鉴权文件（文本 / JSON / Compose）',self._import_lyrics_auth,'import_lyrics_auth')
    def _roon_checked(self,result):
        result=result.get('result',result) if isinstance(result,dict) else {}
        if result.get('accepted'):
            self.roon_connection.setText('Roon Core / Storage检查已排队，完成后自动更新。');return
        self.result_to_page('日志与报告',result)
        connected=result.get('connected',False);state=result.get('status',result.get('state','未知'));core=result.get('core_info',result.get('core',{}))
        core_name=core.get('display_name',core.get('name',core.get('core_id',''))) if isinstance(core,dict) else str(core)
        storage=result.get('storage_diagnostics',result.get('storage',{}));storage_state=storage.get('status',storage.get('state','待复核')) if isinstance(storage,dict) else str(storage or '待复核')
        connection='Core可连接，等待Roon授权 / 配对' if connected and state in ('AUTH_REQUIRED','PAIRING_REQUIRED','REQUIRES_PAIRING') else 'Core已连接' if connected else 'Core未连接'
        storage_description=str(STATE_LABELS.get(storage_state,storage_state))
        if isinstance(storage,dict) and storage.get('storage_configuration_verified') is False:storage_description+='；当前监听尚未确证'
        self.roon_connection.setText(connection+'：'+str(core_name or STATE_LABELS.get(state,state))+'；Storage日志：'+storage_description+'。诊断详情见日志与报告。')
    def _config_saved(self,result):
        result=result.get('result',result)
        config=result.get('config',result)
        if 'values' in config:self.editor.load(config);self.config_version=config['version'];self.config_values=config['values']
    def _import_pt_cookie(self):
        chosen=QFileDialog.getOpenFileName(self,'选择该站Cookie-Editor JSON文件','','Cookie文件 (*.json *.txt);;所有文件 (*)')[0]
        if chosen:self.request('import_pt_cookies',{'site':self.cookie_site.currentData(),'path':chosen},self._config_saved)
    def _import_lyrics_auth(self):
        chosen=QFileDialog.getOpenFileName(self,'选择歌词服务鉴权文件','','鉴权文件 (*.txt *.json *.yaml *.yml);;所有文件 (*)')[0]
        if chosen:self.request('import_lyrics_auth',{'path':chosen},self._config_saved)
        self.request('get_status',{'compact':True},self._status)
    def _save_secret(self):
        value=self.secret_value.text()
        if not value or not self.secret_ref.text().strip() or not self.secret_host.text().strip():
            self.statusBar().showMessage('请填写凭据、引用和绑定域名后保存。');return
        self.secret_value.clear()
        if self.secret_purpose.currentData()=='qbt':value=json.dumps({'username':self.secret_username.text(),'password':value})
        kind,ref=self.secret_purpose.currentData(),self.secret_ref.text().strip()
        self.request('set_secret',{'ref':ref,'host':self.secret_host.text().strip(),'value':value},lambda r:self._secret_saved(kind,ref))
    def _secret_saved(self,kind,ref):
        path={'lyrics':('lyrics','primary','auth_secret_ref'),'ai':('ai','auth_secret_ref'),'qbt':('qbt','credential_ref'),'discogs':('catalog','discogs','credential_ref'),'navidrome':('music_servers','navidrome','password_ref')}.get(kind)
        control=self.editor.controls.get(path,(None,None))[0]
        if control is not None and control.text()!=ref:
            control.setText(ref);self.statusBar().showMessage('凭据已保护保存；点击“校验并保存全部配置”应用新的凭据引用。')
        else:self.statusBar().showMessage('凭据已保护保存；仅显示引用与修订号。')
    def _secret_purpose_changed(self):
        from urllib.parse import urlsplit
        kind=self.secret_purpose.currentData();self.secret_username.setEnabled(kind=='qbt');self.secret_value.clear()
        if kind=='custom':return
        defaults={'lyrics':('vault://lyrics/private/authorization','localhost'),'ai':('vault://ai/compatible/key','localhost'),'qbt':('vault://qbt/local/credentials','127.0.0.1'),'discogs':('vault://catalog/discogs/token','api.discogs.com'),'navidrome':('vault://navidrome/local/password','localhost')}
        ref,host=defaults[kind]
        cfg=self.editor.collect() if self.editor.values else self.config_values
        group=cfg.get('lyrics',{}).get('primary',{}) if kind=='lyrics' else cfg.get('catalog',{}).get('discogs',{}) if kind=='discogs' else cfg.get('music_servers',{}).get('navidrome',{}) if kind=='navidrome' else cfg.get(kind,{})
        ref=group.get('credential_ref' if kind in ('qbt','discogs') else 'password_ref' if kind=='navidrome' else 'auth_secret_ref') or ref
        try:host=urlsplit(group.get('base_url',group.get('url',''))).hostname or host
        except (TypeError,ValueError):host=''
        self.secret_ref.setText(ref);self.secret_host.setText(host)
    def _export_config(self):
        chosen=QFileDialog.getSaveFileName(self,'导出脱敏配置',str(self.base/'reports'/'config.json'),'JSON (*.json)')[0]
        if chosen:self.request('export_config',{'path':chosen},lambda r:self.statusBar().showMessage(pretty(r)))
    def _import_config(self):
        chosen=QFileDialog.getOpenFileName(self,'导入脱敏配置',str(self.base),'JSON (*.json)')[0]
        if chosen:self.request('import_config',{'path':chosen,'expected_version':self.config_version},self._config_saved)
    def _reports(self):
        page='日志与报告';row=self.controls(page);self.button(row,'读取业务事件',lambda:self.request('events',{'after':0},lambda r:self.result_to_page(page,r)),'events');self.button(row,'导出脱敏批次报告',lambda:self.request('export_report',{},lambda r:self.result_to_page(page,r)),'export_report');self.table(page);self.detail(page)
    def closeEvent(self,event):
        self.timer.stop()
        if self.pending:
            self.statusBar().showMessage('后台请求仍在执行，稍后可关闭；任务状态不会重置。');event.ignore();self.timer.start();return
        keep=self.config_values.get('worker',{}).get('keep_background',True)
        if not keep:
            event.ignore();self.request('shutdown_worker',{},lambda r:self._close_after_shutdown(r));return
        self.pool.waitForDone(100);event.accept()
    def _close_after_shutdown(self,result):
        self.config_values.setdefault('worker',{})['keep_background']=True;self.close()

def main(argv=None):
    from musicops.startup import default_profile,remember_profile
    parser=argparse.ArgumentParser(description='MusicOps正式联网桌面客户端');parser.add_argument('--base');parser.add_argument('--demo',action='store_true',help='启动独立演示档案');parser.add_argument('--screenshot');parser.add_argument('--quit-after',type=int,default=0);args=parser.parse_args(argv)
    base=Path(args.base).resolve() if args.base else default_profile()
    if args.demo and not args.base:base=base/'demo-profile'
    if not args.demo:remember_profile(base)
    app=QApplication.instance() or QApplication(sys.argv[:1]);app.setApplicationName('MusicOps')
    # Use the user's installed Windows CJK font; offscreen Qt does not discover
    # Windows fallback fonts reliably. No proprietary font is redistributed.
    if os.name=='nt':
        font_path=Path(os.environ.get('SystemRoot','C:/Windows'))/'Fonts'/'msyh.ttc'
        if font_path.is_file():QFontDatabase.addApplicationFont(str(font_path))
        app.setFont(QFont('Microsoft YaHei',10))
    app.setStyle('Fusion');app.setStyleSheet('QMainWindow{background:#f4f6fa} QListWidget{background:#15253b;color:#edf4ff;border:0;font-size:15px} QListWidget::item{padding:14px 9px} QListWidget::item:selected{background:#2b65b1} QPushButton{padding:7px 12px} QGroupBox{font-weight:600} QTableWidget{background:white} QLabel#status_banner{background:#dce9f9;padding:9px;font-weight:600}')
    window=MainWindow(base,demo_profile=args.demo,online_mode=not args.demo);window.show()
    if args.screenshot:
        QTimer.singleShot(2500,lambda:window.grab().save(args.screenshot))
    if args.quit_after:QTimer.singleShot(args.quit_after,app.quit)
    return app.exec()

if __name__=='__main__':raise SystemExit(main())
