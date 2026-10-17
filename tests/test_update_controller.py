from PySide6.QtCore import QTimer
from PySide6.QtWidgets import QApplication, QMainWindow, QLabel
from musicops.gui.updates import UpdateController


class Owner(QMainWindow):
    def __init__(self):
        super().__init__();self.config={'updates':{'enabled':True}};self.calls=[];self.update_note=QLabel();self.timer=QTimer(self);self.closed=False
    def call(self,command,payload,done=None,failed=None):self.calls.append((command,payload,done,failed))
    def closeEvent(self,event):self.closed=True;event.accept()


def test_update_waits_for_successful_worker_exit_before_launching_installer():
    app=QApplication.instance() or QApplication([]);owner=Owner();launched=[]
    controller=UpdateController(owner,installer=lambda update:launched.append(update))
    controller.ready('synthetic verified update')
    assert not launched and owner.calls[-1][0]=='shutdown_worker'
    owner.calls[-1][3]('当前任务正在处理文件');assert not launched and not owner.closed
    controller.try_install();owner.calls[-1][2]({'stopped':True})
    assert launched==['synthetic verified update'] and owner.closed and not controller.wait.isActive()


def test_disabling_automatic_updates_prevents_pending_install_but_allows_explicit_manual_update():
    app=QApplication.instance() or QApplication([]);owner=Owner();owner.config['updates']['enabled']=False;launched=[]
    controller=UpdateController(owner,installer=lambda update:launched.append(update));controller.automatic=True
    controller.ready('synthetic update');assert not owner.calls and not launched
    controller.automatic=False;controller.ready('synthetic update');assert owner.calls[-1][0]=='shutdown_worker'
    owner.calls[-1][2]({'stopped':True});assert launched==['synthetic update']
