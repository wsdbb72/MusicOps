"""Explicit credential domains and human-facing instructions for playlist readers."""
import json
from musicops.clients.vault import import_cookies

HOSTS={
    'qqmusic':['c.y.qq.com','u.y.qq.com'],'qishui':['beta-luna.douyin.com'],
    'kugou':['mobilecdn.kugou.com','m.kugou.com','www.kugou.com','pubsongscdn.kugou.com','mobiles.kugou.com'],
    'kuwo':['m.kuwo.cn'],'migu':['app.c.nf.migu.cn'],
    'bilibili':['api.bilibili.com'],'spotify':['api.spotify.com'],
    'applemusic':['api.music.apple.com'],'youtube':['www.googleapis.com'],
}
HELP={
    'qqmusic':'可选：QQ 音乐 Cookie-Editor JSON。公开歌单通常无需登录；只有匹配 API 域名的 Cookie 会被使用。',
    'qishui':'可选：汽水音乐 Cookie-Editor JSON。公开歌单先尝试直接读取；登录态必须匹配官方接口域名。',
    'kugou':'可选：酷狗 Cookie-Editor JSON。部分个人云歌单需要账号授权；可导入客户端或 Playlist Out 导出文件。',
    'kuwo':'可选：酷我 Cookie-Editor JSON。公开歌单先尝试直接读取。',
    'migu':'可选：咪咕 Cookie-Editor JSON。公开歌单先尝试直接读取。',
    'bilibili':'可选：bilibili Cookie-Editor JSON（发送到 api.bilibili.com）。收藏夹可读视频信息；标题、UP 主不等于音乐艺人或专辑，需要核对。',
    'spotify':'可选：Spotify Access Token 原文，或 JSON {"access_token":"…","market":"US"}。官方接口仅允许账号可读取的歌单；未填时尝试公开预览，可能不完整。',
    'applemusic':'可选：Apple Music Developer Token（JWT）原文，或 JSON {"developer_token":"…"}。公开网页可读时无需填写；接口访问取决于开发者授权。',
    'youtube':'可选：YouTube Data API Key 原文，或 JSON {"api_key":"…"}。OAuth 可用 {"access_token":"…"}；未填时尝试公开页面。视频可能缺少可确认的艺人与专辑。',
}


def credential_hosts(platform):
    if platform not in HOSTS:raise ValueError('未知歌单平台')
    return list(HOSTS[platform])


def validate_credential(platform,text):
    if not isinstance(text,str) or not text.strip() or len(text.encode('utf-8'))>1_000_000:raise ValueError('歌单平台登录信息需为不超过 1MB 的文本')
    if platform in ('spotify','applemusic','youtube'):
        text=text.strip()
        if text.startswith('{'):
            data=json.loads(text)
            keys={'spotify':('access_token',),'applemusic':('developer_token',),'youtube':('api_key','access_token')}[platform]
            if not isinstance(data,dict) or not any(isinstance(data.get(key),str) and data[key].strip() for key in keys):raise ValueError('请按平台说明填写有效的 Token 或 API Key 字段')
        elif any(char in text for char in ('\n','\r','\x00')):raise ValueError('Token 或 API Key 须为单行原文')
        return text
    data=json.loads(text)
    if isinstance(data,dict) and platform=='bilibili':data=data.get('cookies')
    if not isinstance(data,list):raise ValueError('请使用 Cookie-Editor 导出的 JSON 数组')
    selected={}
    for host in credential_hosts(platform):
        try:items=import_cookies(json.dumps(data),host)
        except ValueError:continue
        for item in items:selected[(item['domain'],item.get('path','/'),item['name'])]=item
    if not selected:raise ValueError('Cookie 域名与该平台 API 不匹配；公开歌单可先留空，或导入平台导出的歌单文件')
    return json.dumps(list(selected.values()),ensure_ascii=False)
