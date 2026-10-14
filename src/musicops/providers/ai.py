from __future__ import annotations
import json,re
from urllib.parse import urlparse
from musicops.clients.http import HttpError

class AIProvider:
    """OpenAI-compatible Responses client; plans only, never direct filesystem or download."""
    def __init__(self,config,transport,secret_getter): self.config, self.transport, self.secret_getter=config,transport,secret_getter
    def _request(self,payload):
        base=self.config['base_url'].rstrip('/')
        if self.config['protocol']!='responses': raise ValueError('AI_PROTOCOL_UNSUPPORTED')
        u=urlparse(base)
        if u.scheme!='https' or not u.hostname or u.username or u.password or u.query or u.fragment: raise ValueError('AI_BASE_MUST_CLEAN_HTTPS')
        if not self.config['model']: raise ValueError('AI_MODEL_REQUIRED')
        key=self.secret_getter(self.config['auth_secret_ref'],urlparse(base).hostname)
        if not isinstance(key,str) or not key.strip():raise HttpError('AUTH_REQUIRED','AI密钥未配置')
        body=dict(payload); body.update(model=self.config['model'],max_output_tokens=self.config['max_output_tokens'])
        if 'temperature' in self.config: body['temperature']=self.config['temperature']
        headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'}
        endpoint=base if base.endswith('/responses') else base+'/responses'
        content=json.dumps(body,ensure_ascii=False).encode()
        if len(content)>131072:raise HttpError('INPUT_TOO_LARGE','AI上下文超过本机请求预算；请缩小分析范围')
        response=self.transport.request('POST',endpoint,headers=headers,content=content,timeout=self.config['timeout'])
        if response.status_code in (401,403): raise HttpError('AUTH_FAILED','AI鉴权失败')
        if response.status_code==429: raise HttpError('RATE_LIMITED','AI限流')
        if response.status_code>=500: raise HttpError('TRANSIENT_ERROR','AI服务暂时不可用')
        if response.status_code!=200: raise HttpError('API_CONTRACT_ERROR','AI返回契约错误')
        return response.json()
    @staticmethod
    def _output(data):
        if not isinstance(data,dict): raise ValueError('AI_INVALID_RESPONSE')
        if data.get('status') not in (None,'completed'): raise ValueError('AI_STATUS_'+str(data.get('status')))
        text=''
        if not isinstance(data.get('output',[]),list):raise ValueError('AI_INVALID_RESPONSE')
        for item in data.get('output',[]):
            if not isinstance(item,dict):raise ValueError('AI_INVALID_RESPONSE')
            for part in item.get('content',[]) if isinstance(item.get('content'),list) else []:
                if not isinstance(part,dict):raise ValueError('AI_INVALID_RESPONSE')
                if part.get('type')=='refusal':raise ValueError('AI_REFUSED')
                if part.get('type')=='output_text':
                    if not isinstance(part.get('text'),str):raise ValueError('AI_INVALID_RESPONSE')
                    text+=part['text']
        if not text.strip(): raise ValueError('AI_EMPTY_OUTPUT')
        match=re.fullmatch(r'\s*```(?:json)?\s*\n?(.*?)\n?```\s*',text,re.S)
        if match:text=match.group(1)
        def invalid_constant(value):raise ValueError('AI_INVALID_RESPONSE')
        try:return json.loads(text,parse_constant=invalid_constant)
        except Exception:
            return {'answer':text.strip(),'structured':False}
    def analyze(self,context,instructions=''):
        from musicops.clients.http import redact
        context=redact(context)
        payload={'input':[{'role':'system','content':'你是音乐库分析助手。只根据给定JSON事实分析，不编造目录、凭据或站点状态。输出中文。'},{'role':'user','content':json.dumps({'context':context,'instructions':instructions},ensure_ascii=False)}]}
        output=self._output(self._request(payload))
        return output if isinstance(output,dict) else {'answer':output}
    def plan(self,message,state):
        from musicops.clients.http import redact
        state=redact(state)
        schema={'type':'object','additionalProperties':False,'required':['intent','proposed_actions','requires_review','explanation'],'properties':{'intent':{'type':'string','enum':['search','fill','repair','dedup','rollback','analyze']},'proposed_actions':{'type':'array','items':{'type':'object','additionalProperties':False,'required':['action','target_id','reason'],'properties':{'action':{'type':'string'},'target_id':{'type':'string'},'reason':{'type':'string'}}}},'requires_review':{'type':'boolean'},'explanation':{'type':'string'}}}
        payload={'input':[{'role':'system','content':'你是MusicOps计划助手。只能根据state生成可审核计划，不执行任何动作。只返回JSON对象，必须符合此schema：'+json.dumps(schema,ensure_ascii=False)},{'role':'user','content':json.dumps({'message':message,'state':state},ensure_ascii=False)}]}
        if self.config.get('structured_outputs'): payload['text']={'format':{'type':'json_schema','name':'musicops_plan','strict':True,'schema':schema}}
        result=self._output(self._request(payload))
        required={'intent','proposed_actions','requires_review','explanation'}
        if not isinstance(result,dict) or set(result)!=required or result['intent'] not in ('search','fill','repair','dedup','rollback','analyze') or not isinstance(result['proposed_actions'],list) or type(result['requires_review']) is not bool or not isinstance(result['explanation'],str):raise ValueError('AI_INVALID_PLAN')
        for action in result['proposed_actions']:
            if not isinstance(action,dict) or set(action)!={'action','target_id','reason'} or not all(isinstance(v,str) for v in action.values()):raise ValueError('AI_INVALID_PLAN_ACTION')
        result['requires_review']=True
        return result
