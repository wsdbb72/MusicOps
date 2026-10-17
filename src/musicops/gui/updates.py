"""Nonblocking download, then wait for the worker's safe exit before installing."""
from PySide6.QtCore import QObject, QRunnable, Signal, QTimer
from musicops.updates import Updater, launch_installer


class Signals(QObject):
    progress = Signal(str)
    done = Signal(object)
    error = Signal(str)


class Download(QRunnable):
    def __init__(self, updater):
        super().__init__(); self.updater = updater; self.signals = Signals()
    def run(self):
        try: self.signals.done.emit(self.updater.stage_latest(self.signals.progress.emit))
        except Exception as exc:
            from urllib.error import HTTPError
            self.signals.error.emit('GitHub 限流，请稍后重试' if isinstance(exc, HTTPError) and exc.code in (403, 429) else str(exc) if isinstance(exc, ValueError) else '连接 GitHub 未完成，可稍后点击“检查更新”重试')


class UpdateController(QObject):
    def __init__(self, owner, updater=None, installer=None):
        super().__init__(owner); self.owner = owner; self.updater = updater or Updater(); self.installer = installer or launch_installer
        self.download = None; self.update = None; self.exiting = False; self.requested = False
        self.wait = QTimer(self); self.wait.setInterval(5000); self.wait.timeout.connect(self.try_install)
    def start(self,automatic=True):
        if self.download or self.update: return
        self.automatic=automatic
        self.download = Download(self.updater); self.download.signals.progress.connect(self.owner.update_note.setText)
        self.download.signals.done.connect(self.ready); self.download.signals.error.connect(self.failed)
        self.owner.pool.start(self.download)
    def failed(self, error):
        self.download = None; self.owner.update_note.setText('更新检查：' + error)
    def ready(self, update):
        self.download = None
        if update is None: self.owner.update_note.setText('MusicOps 已是最新版本'); return
        self.update = update; self.wait.start(); self.try_install()
    def try_install(self):
        if self.requested or not self.update: return
        if getattr(self,'automatic',True) and not self.owner.config.get('updates',{}).get('enabled',True):
            self.wait.stop();self.update=None;self.owner.update_note.setText('已关闭自动更新；下载缓存保留，可手动检查更新');return
        self.requested = True; self.owner.update_note.setText('新版已下载并校验，正在等待后台安全退出后安装…')
        self.owner.call('shutdown_worker', {}, self.stopped, self.busy)
    def busy(self, message):
        self.requested = False
        self.owner.update_note.setText('新版已下载；当前步骤完成后自动安装。音乐文件与任务记录保留。')
    def stopped(self, result):
        self.wait.stop(); self.owner.timer.stop()
        try:
            self.installer(self.update)
        except Exception:
            self.requested = False; self.update = None; self.owner.timer.start(); self.owner.update_note.setText('安装器未能启动，请点击“检查更新”重试，或从 Release 手动运行安装包'); return
        self.owner.close()
