"""Independent site results: an empty search is successful, not a site failure."""
import re
from musicops.clients.http import HttpError, PolicyBlocked

SITE_NAMES = {'open.cd': 'OpenCD', 'dicmusic': 'DicMusic', 'orpheus': 'Orpheus', 'redacted': 'RED'}
SUCCESS = {'OK', 'NO_RESOURCE', 'NO_MATCH'}


def edition_conflict(observed, expected):
    """Extra version markings cannot become an automatic same-edition hit."""
    markers = (r'\blive\b', r'\bremix(?:ed)?\b', r'\bremaster(?:ed)?\b', r'\bdeluxe\b',
               r'\banniversary\b', r'\binstrumental\b', r'\bkaraoke\b', r'\bre[- ]?record(?:ed|ing)\b',
               '现场', '重制', '重录', '混音', '周年', '豪华')
    return any(re.search(marker, observed, re.I) and not re.search(marker, expected, re.I) for marker in markers)
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


def search_album(factory, query, album='', checkpoint=None):
    """One exact query, then one title-only query only after a successful empty page.

    Broader results are candidates, never evidence that identity is confirmed.
    Rate/auth/network failures do not trigger an immediate retry storm.
    """
    queries = [query]
    if album and album.strip() != query.strip():
        queries.append(album.strip())
    attempts = []; candidates = {}; result = {}
    for keyword in queries:
        if checkpoint: checkpoint()
        result = search_site(factory, keyword)
        attempts.append({'query': keyword, 'status': result.get('status')})
        for candidate in result.get('candidates', []):
            candidates.setdefault((candidate.get('source'), candidate.get('candidate_id')), candidate)
        if candidates or result.get('status') not in SUCCESS or result.get('search_complete') is False:
            break
    return {**result, 'candidates': list(candidates.values()), 'attempts': attempts}


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
