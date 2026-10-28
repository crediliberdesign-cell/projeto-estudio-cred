"""Opt-in integration runner. Requires FFMPEG and FFPROBE env paths; synthetic media is CC0."""
import os,json,subprocess,sys,tempfile,hashlib
from pathlib import Path
worker=Path(__file__).resolve().parents[1]/'worker.py'
root=Path(sys.argv[1] if len(sys.argv)>1 else tempfile.mkdtemp(prefix='media-integration-')).resolve();root.mkdir(parents=True,exist_ok=True)
ffmpeg=os.environ['FFMPEG'];ffprobe=os.environ['FFPROBE']
source=root/'fonte-áudio.mp4'
subprocess.run([ffmpeg,'-hide_banner','-loglevel','error','-y','-f','lavfi','-i','testsrc2=size=640x360:rate=30:duration=6','-f','lavfi','-i',"aevalsrc=if(between(t\\,2\\,4)\\,0\\,0.1*sin(2*PI*440*t)):s=48000:d=6",'-c:v','libx264','-pix_fmt','yuv420p','-c:a','aac','-shortest',str(source)],check=True)
hash_before=hashlib.sha256(source.read_bytes()).hexdigest()
base={'input':str(source),'ffmpeg':ffmpeg,'ffprobe':ffprobe,'allowed_roots':[str(root)],'project':{'aspect':'9:16','resolution':360,'audio':{'mode':'original'},'brand_snapshot':{'name':'Empresa A','colors':[{'role':'accent','hex':'#FF8A00'}]}}}
results={}
def run(action,options=None,project=None):
    out=root/action;out.mkdir(exist_ok=True)
    req={**base,'action':action,'output_dir':str(out),'options':options or {}}
    if project: req['project']=project
    request=out/'request.json';request.write_text(json.dumps(req,ensure_ascii=False))
    proc=subprocess.run([sys.executable,str(worker),'--request',str(request)],capture_output=True,text=True)
    events=[json.loads(x) for x in proc.stdout.splitlines() if x.startswith('{')]
    if proc.returncode: raise AssertionError(events[-1] if events else proc.stderr)
    result=events[-1];assert result['type']=='result';results[action]=result;return result
meta=run('probe');assert meta['duration']==6 and meta['has_audio']
plan=run('plan_silence');assert 1.4<plan['removed_seconds']<2.1
transcript={'segments':[{'start':0.2,'end':1.5,'text':'Olá! Português com ação.','words':[{'start':.2,'end':.5,'word':'Olá!'},{'start':.6,'end':.9,'word':'Português'},{'start':1,'end':1.2,'word':'com'},{'start':1.2,'end':1.5,'word':'ação.'}]},{'start':4.2,'end':5.5,'text':'Marca B: revisão final.','words':[{'start':4.2,'end':4.5,'word':'Marca'},{'start':4.5,'end':4.7,'word':'B:'},{'start':4.8,'end':5.1,'word':'revisão'},{'start':5.2,'end':5.5,'word':'final.'}]}]}
cap=run('captions',{'transcript':transcript});assert 'Português' in Path(cap['srt']).read_text()
edited=run('edit',{'ranges':[{'start':0,'end':2},{'start':4,'end':6}],'captions':True,'transcript':transcript,'burn_captions':True})
assert abs(edited['probe']['duration']-4)<.1 and edited['probe']['width']==360 and edited['probe']['height']==640 and edited['original_unchanged']
assert edited['probe']['has_audio']
# Different individual project's mute/square choices must not inherit first render.
base['project']={'aspect':'1:1','resolution':360,'duration':2,'audio':{'mode':'none'},'brand_snapshot':{'name':'Empresa A'}}
muted=run('edit',{'preview':True});assert not muted['probe']['has_audio'] and muted['probe']['width']==muted['probe']['height']==360 and abs(muted['probe']['duration']-2)<.1
assert hashlib.sha256(source.read_bytes()).hexdigest()==hash_before
sample=run('frames',{'count':3});assert len(sample['frames'])==3
(root/'results.json').write_text(json.dumps({'status':'passed','platform':sys.platform,'results':results},ensure_ascii=False,indent=2))
print(json.dumps({'status':'passed','results_file':str(root/'results.json'),'tests':['probe','silence_handles','accented_captions','cut_caption_audio_sync','independent_project_mute_square','original_hash','frame_sampling']},ensure_ascii=False))
