"""MusicOps desktop entry point."""
import argparse,os,sys
from pathlib import Path
from PySide6.QtCore import QTimer,QTranslator,QLibraryInfo
from PySide6.QtGui import QFont,QFontDatabase
from PySide6.QtWidgets import QApplication
from musicops.gui.workbench import Workbench,STYLE

def main(argv=None):
    from musicops.startup import default_profile,remember_profile
    from musicops.gui.branding import app_icon,set_windows_identity,apply_native_titlebar
    parser=argparse.ArgumentParser(description='MusicOps 音乐库管理');parser.add_argument('--base');parser.add_argument('--screenshot');parser.add_argument('--quit-after',type=int,default=0);args=parser.parse_args(argv)
    base=Path(args.base).resolve() if args.base else default_profile();remember_profile(base)
    set_windows_identity()
    app=QApplication.instance() or QApplication(sys.argv[:1]);app.setApplicationName('MusicOps');app.setWindowIcon(app_icon())
    translator=QTranslator(app)
    roots=[QLibraryInfo.path(QLibraryInfo.TranslationsPath),str(Path(getattr(sys,'_MEIPASS',Path.cwd()))/'PySide6/Qt/translations')]
    for directory in roots:
        if translator.load('qtbase_zh_CN',directory):app.installTranslator(translator);break
    app._musicops_translator=translator
    if os.name=='nt':
        font_path=Path(os.environ.get('SystemRoot','C:/Windows'))/'Fonts'/'msyh.ttc'
        if font_path.is_file():QFontDatabase.addApplicationFont(str(font_path))
        app.setFont(QFont('Microsoft YaHei',10))
    app.setStyle('Fusion');app.setStyleSheet(STYLE);window=Workbench(base,updates_enabled=bool(getattr(sys,'frozen',False) and not args.quit_after and not args.screenshot));window.setWindowIcon(app_icon());window.show();apply_native_titlebar(window)
    if args.screenshot:
        screenshot=Path(args.screenshot).resolve();screenshot.parent.mkdir(parents=True,exist_ok=True);QTimer.singleShot(2500,lambda:window.grab().save(str(screenshot)))
    if args.quit_after:QTimer.singleShot(args.quit_after,app.quit)
    return app.exec()

if __name__=='__main__':raise SystemExit(main())
