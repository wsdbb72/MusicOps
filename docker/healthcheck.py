import urllib.request
from musicops.worker.client import WorkerClient
# A container-wide HTTP(S)_PROXY must never route the loopback probe away.
with urllib.request.build_opener(urllib.request.ProxyHandler({})).open('http://127.0.0.1:8080/healthz',timeout=3) as response:
    assert response.status==200
WorkerClient('/config/profile',timeout=5).call('get_status',{'compact':True})
