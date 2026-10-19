"""Responsive action rows: wrap controls instead of clipping narrow windows."""
from PySide6.QtCore import QRect,QSize,Qt
from PySide6.QtWidgets import QLayout

class WrapLayout(QLayout):
    def __init__(self,parent=None):
        super().__init__(parent);self.items=[];self.setContentsMargins(0,0,0,0);self.setSpacing(8)
    def addItem(self,item):self.items.append(item)
    def addStretch(self,*args):pass
    def count(self):return len(self.items)
    def itemAt(self,index):return self.items[index] if 0<=index<len(self.items) else None
    def takeAt(self,index):return self.items.pop(index) if 0<=index<len(self.items) else None
    def expandingDirections(self):return Qt.Orientations()
    def hasHeightForWidth(self):return True
    def heightForWidth(self,width):return self._arrange(QRect(0,0,width,0),False)
    def setGeometry(self,rect):super().setGeometry(rect);self._arrange(rect,True)
    def sizeHint(self):return self.minimumSize()
    def minimumSize(self):
        result=QSize()
        for item in self.items:result=result.expandedTo(item.minimumSize())
        margins=self.contentsMargins();return result+QSize(margins.left()+margins.right(),margins.top()+margins.bottom())
    def _arrange(self,rect,apply):
        margins=self.contentsMargins();area=rect.adjusted(margins.left(),margins.top(),-margins.right(),-margins.bottom());x=area.x();y=area.y();height=0
        for item in self.items:
            if item.isEmpty():continue
            size=item.sizeHint();width=min(size.width(),area.width())
            if x>area.x() and x+width>area.right()+1:x=area.x();y+=height+self.spacing();height=0
            if apply:item.setGeometry(QRect(x,y,width,size.height()))
            x+=width+self.spacing();height=max(height,size.height())
        return y+height-rect.y()+margins.bottom()
