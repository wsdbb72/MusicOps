"""Packaged brand assets and native Windows title-bar integration."""
from pathlib import Path
import ctypes,os,sys
from PySide6.QtGui import QIcon


def asset_path(name):
    return Path(__file__).resolve().parents[1]/'assets'/name


def app_icon():
    return QIcon(str(asset_path('musicops.ico')))


def apply_native_titlebar(window):
    if os.name!='nt':return
    try:
        hwnd=int(window.winId())
        dwm=ctypes.windll.dwmapi
        # Keep native snapping, resizing and system buttons while matching the
        # workbench palette. Older Windows versions may ignore these attributes.
        for attribute,value in ((20,1),(35,0x00221910),(36,0x00F5EEE6)):
            color=ctypes.c_int(value)
            dwm.DwmSetWindowAttribute(ctypes.c_void_p(hwnd),attribute,ctypes.byref(color),ctypes.sizeof(color))
    except (OSError,AttributeError,ValueError):pass


def set_windows_identity():
    if os.name=='nt':
        try:ctypes.windll.shell32.SetCurrentProcessExplicitAppUserModelID('wsdbb72.MusicOps.Desktop')
        except (OSError,AttributeError):pass
