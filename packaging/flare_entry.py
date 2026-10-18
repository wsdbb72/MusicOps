"""FlareSolverr launcher for MusicOps' private, self-contained browser runtime."""
import hmac
import os
from pathlib import Path
import runpy
import shutil
import sys
import multiprocessing

multiprocessing.freeze_support()

import utils
import bottle

# The upstream detached launcher uses multiprocessing.spawn on macOS. Frozen
# services must launch Chrome directly, in our owned process group, rather than
# recursively re-executing this service while waiting for a browser PID.
chrome_init = utils.uc.Chrome.__init__
def managed_chrome_init(self, *args, **kwargs):
    kwargs.setdefault('use_subprocess', True)
    chrome_init(self, *args, **kwargs)
utils.uc.Chrome.__init__ = managed_chrome_init

root = Path(sys.executable).resolve().parent
data = Path(os.environ['MUSICOPS_ENGINE_DATA'])
driver_name = 'chromedriver.exe' if os.name == 'nt' else 'chromedriver'
driver = data / driver_name
shutil.copy2(root / 'driver' / driver_name, driver)
driver.chmod(0o700)
utils.PATCHED_DRIVER_PATH = str(driver)
utils.FLARESOLVERR_VERSION = '3.5.2'
utils.CHROME_MAJOR_VERSION = '155'
utils.CHROME_EXE_PATH = str(root / 'browser' / ('chrome.exe' if os.name == 'nt' else 'Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing'))
if sys.platform == 'darwin':
    # macOS has native headless Chrome and no X11/Xvfb dependency.
    utils.start_xvfb_display = lambda: None

original = bottle.Bottle.__call__
def authenticated(self, environ, start_response):
    expected = os.environ.get('MUSICOPS_SERVICE_TOKEN', '')
    if not expected or not hmac.compare_digest(environ.get('HTTP_X_MUSICOPS_SERVICE', ''), expected):
        start_response('403 Forbidden', [('Content-Type', 'text/plain')])
        return [b'Forbidden']
    return original(self, environ, start_response)
bottle.Bottle.__call__ = authenticated
runpy.run_module('flaresolverr', run_name='__main__')
