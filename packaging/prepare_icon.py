"""Convert the reviewed source PNG into Windows' standard multi-size ICO."""
from pathlib import Path
from PIL import Image

root=Path(__file__).resolve().parent.parent
source=root/'src/musicops/assets/musicops.png'
with Image.open(source) as image:
    image.convert('RGBA').save(source.with_suffix('.ico'),sizes=[(s,s) for s in (16,20,24,32,40,48,64,128,256)])
print('Created multi-size Windows icon')
