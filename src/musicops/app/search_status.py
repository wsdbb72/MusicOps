"""Independent site results: an empty search is successful, not a site failure."""
from musicops.clients.http import HttpError, PolicyBlocked

SITE_NAMES = {'open.cd': 'OpenCD', 'dicmusic': 'DicMusic', 'orpheus': 'Orpheus', 'redacted': 'RED'}
SUCCESS = {'OK', 'NO_RESOURCE', 'NO_MATCH'}
MESSAGES = {
    'AUTH_REQUIRED': '尚未填写 Cookie', 'AUTH_FAILED': 'Cookie 已失效或登录被拒绝',
    'RATE_LIMITED': '站点限流，请稍后重试', 'ACCESS_BLOCKED': '站点访问受限',
    'NETWORK_ERROR': '连接失败', 'TRANSIENT_ERROR': '站点暂时不可用',
    'PARSE_ERROR': '站点页面暂时无法解析', 'API_CONTRACT_ERROR': '站点返回格式改变',
    'POLICY_BLOCKED': '站点连接设置未通过', 'DISABLED': '未启用',
}


def search_site(factory, query):
    """Isolate one site's construction/network failure without hiding cancellation."""
    try:
        return factory().search(query)
    except (HttpError, PolicyBlocked, OSError, TimeoutError) as exc:
        return {'status': getattr(exc, 'error_class', 'NETWORK_ERROR'), 'candidates': []}


def summarize(outcomes):
    successful = []; unavailable = []; limited = []
    for site, result in outcomes.items():
        status = result.get('status'); name = SITE_NAMES.get(site, site)
        if status == 'DISABLED':
            result['message'] = '未启用'; continue
        if status in SUCCESS:
            successful.append(site)
            # NO_RESOURCE proves an empty result even with no pagination field.
            result['message'] = '未找到资源' if status in {'NO_RESOURCE', 'NO_MATCH'} else '检索成功'
            if status == 'OK' and result.get('search_complete') is False:
                limited.append(site); result['message'] += '（当前为首页结果）'
        else:
            unavailable.append(site); result['message'] = MESSAGES.get(status, '检索暂时不可用')
    warnings = [SITE_NAMES.get(s, s) + '：' + outcomes[s]['message'] for s in unavailable + limited]
    return {'successful_sites': successful, 'unavailable_sites': unavailable,
            'limited_sites': limited, 'warnings': warnings}


def detail(summary):
    return '；'.join(summary['warnings'])
