"""Consistent list popups without native menu scroller gutters."""
from PySide6.QtCore import Qt
from PySide6.QtGui import QColor, QPalette
from PySide6.QtWidgets import QComboBox as NativeComboBox, QListView, QFrame


class QComboBox(NativeComboBox):
    def __init__(self, parent=None):
        super().__init__(parent)
        view = QListView(self)
        view.setFrameShape(QFrame.NoFrame)
        view.setVerticalScrollMode(QListView.ScrollPerPixel)
        view.setHorizontalScrollBarPolicy(Qt.ScrollBarAlwaysOff)
        self.setView(view)
        self.setMaxVisibleItems(10)

    def showPopup(self):
        view = self.view()
        palette = view.palette()
        for role in (QPalette.Window, QPalette.Base, QPalette.Button):
            palette.setColor(role, QColor('#14243a'))
        view.setPalette(palette)
        view.viewport().setPalette(palette)
        # The popup's private QFrame is outside the styled view; its native
        # window background otherwise exposes white strips above/below rows.
        super().showPopup()
        container = view.window()
        container.setPalette(palette)
        container.setAutoFillBackground(True)
        container.setContentsMargins(0, 0, 0, 0)
        container.setStyleSheet('QFrame { background: #14243a; border: 0; }')
