"""Local deterministic media worker. JSON-lines protocol; never executes generated shell.
Copyright 2026. Application code under MIT; third-party binaries retain own licenses.
"""
from __future__ import annotations
import argparse, hashlib, json, math, os, queue, re, shutil, subprocess, sys, tempfile, threading, time
from pathlib import Path
from fractions import Fraction

# Retain DLL directory handles for the lifetime of CTranslate2 (Windows 3.8+).
_DLL_HANDLES = []
if os.name == 'nt':
    import site
    for base in site.getsitepackages():
        for name in ('cublas', 'cudnn', 'cuda_runtime', 'cuda_nvrtc'):
            directory = Path(base) / 'nvidia' / name / 'bin'
            if directory.is_dir():
                _DLL_HANDLES.append(os.add_dll_directory(str(directory)))

class MediaError(Exception): pass

def emit(kind, **fields):
    print(json.dumps({'type': kind, **fields}, ensure_ascii=False), flush=True)

def progress(stage, value=None, **fields):
    emit('progress', stage=stage, **({'progress': round(value, 3)} if value is not None else {}), **fields)

def number(v, default, low, high):
    if v is None or v == '': v = default
    try: f = float(v)
    except (ValueError, TypeError): raise MediaError('Valor numérico inválido.')
    if not math.isfinite(f) or not low <= f <= high: raise MediaError(f'Valor fora do intervalo {low}–{high}.')
    return f

def read_json(path):
    return json.loads(Path(path).read_text(encoding='utf-8-sig'))

def write_json(path, data):
    Path(path).write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding='utf-8')
    return str(path)

def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''): h.update(chunk)
    return h.hexdigest()

def canonical(path):
    return Path(path).expanduser().resolve(strict=True)

def contains(root, path):
    try: path.relative_to(root); return True
    except ValueError: return False

def source_path(req, path):
    p = canonical(path)
    if not p.is_file(): raise MediaError('Selecione um arquivo de mídia válido.')
    roots = req.get('allowed_roots')
    if roots and not any(p == canonical(r) or contains(canonical(r), p) for r in roots):
        raise MediaError('O arquivo está fora das pastas autorizadas deste trabalho.')
    return p

def check_cancel(req):
    if req.get('cancel_file') and Path(req['cancel_file']).exists(): raise MediaError('Trabalho cancelado.')

def run(req, args, *, stage=None, duration=None, timeout=3600, cwd=None):
    """Launch owned process only, collecting bounded logs and honest FFmpeg progress."""
    check_cancel(req)
    creationflags = subprocess.CREATE_NO_WINDOW if os.name == 'nt' else 0
    p = subprocess.Popen([str(a) for a in args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         encoding='utf-8', errors='replace', cwd=cwd, creationflags=creationflags)
    q = queue.Queue()
    def reader(pipe, tag):
        for line in iter(pipe.readline, ''): q.put((tag, line))
        q.put((tag, None))
    threads = [threading.Thread(target=reader, args=(p.stdout, 'out'), daemon=True),
               threading.Thread(target=reader, args=(p.stderr, 'err'), daemon=True)]
    for t in threads: t.start()
    out, err, closed = [], [], set()
    started, last = time.monotonic(), 0.0
    try:
        while len(closed) < 2:
            check_cancel(req)
            if time.monotonic() - started > timeout: raise MediaError('O processamento excedeu o tempo permitido. Tente um trecho menor.')
            try: tag, line = q.get(timeout=0.15)
            except queue.Empty: continue
            if line is None: closed.add(tag); continue
            target = out if tag == 'out' else err
            target.append(line)
            if len(target) > 8000: del target[:4000]
            if stage and duration and line.startswith('out_time_us='):
                try: value = min(0.999, max(0, float(line.split('=', 1)[1]) / 1000000 / duration))
                except ValueError: continue
                if time.monotonic() - last > 0.3:
                    progress(stage, value); last = time.monotonic()
            elif line.startswith('{'):
                try:
                    event = json.loads(line)
                    if event.get('type') == 'progress': emit('progress', **{k:v for k,v in event.items() if k!='type'})
                except (ValueError, TypeError): pass
        code = p.wait(timeout=5)
        if code: raise MediaError('Falha no processamento: ' + ''.join(err)[-3500:])
        return ''.join(out), ''.join(err)
    finally:
        if p.poll() is None:
            p.terminate()
            try: p.wait(timeout=4)
            except subprocess.TimeoutExpired: p.kill(); p.wait()
        for stream in (p.stdout, p.stderr): stream.close()

def binary(req, name):
    value = req.get(name) or shutil.which(name)
    if not value or not Path(value).is_file(): raise MediaError(f'{name} indisponível. Abra Reparar instalação.')
    return str(Path(value).resolve())

def ffmpeg(req, args, **kw):
    return run(req, [binary(req,'ffmpeg'), '-hide_banner', '-nostdin', '-y', *args], **kw)

def probe(req, path=None):
    p = source_path(req, path or req['input'])
    out, _ = run(req, [binary(req,'ffprobe'), '-v','error','-show_format','-show_streams','-of','json',p], timeout=60)
    raw = json.loads(out)
    videos = [s for s in raw.get('streams',[]) if s.get('codec_type') == 'video' and not s.get('disposition',{}).get('attached_pic')]
    audios = [s for s in raw.get('streams',[]) if s.get('codec_type') == 'audio']
    v = videos[0] if videos else {}
    try: fps = float(Fraction(v.get('avg_frame_rate','0/1')))
    except (ZeroDivisionError, ValueError): fps = 0
    duration = float(raw.get('format',{}).get('duration') or v.get('duration') or (audios[0].get('duration',0) if audios else 0))
    rotation = int(v.get('tags',{}).get('rotate',0))
    for side in v.get('side_data_list',[]):
        if 'rotation' in side: rotation = int(side['rotation'])
    w,h = int(v.get('width',0)),int(v.get('height',0))
    if abs(rotation)%180 == 90: w,h = h,w
    return {'path':str(p),'duration':duration,'width':w,'height':h,'fps':fps,'rotation':rotation,
            'has_video':bool(videos),'has_audio':bool(audios),'video_codec':v.get('codec_name'),
            'audio_tracks':[{'index':s['index'],'codec':s.get('codec_name'),'language':s.get('tags',{}).get('language')} for s in audios],
            'vfr_possible':v.get('r_frame_rate') != v.get('avg_frame_rate'),
            'hdr':v.get('color_transfer') in ('smpte2084','arib-std-b67'),
            'color_transfer':v.get('color_transfer'),'streams':raw.get('streams',[])}

def output_dir(req):
    path = Path(req['output_dir']).resolve()
    path.mkdir(parents=True, exist_ok=True)
    if shutil.disk_usage(path).free < 64*1024*1024: raise MediaError('Espaço insuficiente: libere espaço na unidade de saída.')
    return path

def fps_for(meta):
    fps = meta.get('fps') or 30
    return max(1, min(60, fps))

def timeline(ranges, duration, fps=30):
    result=[]; cursor=0.; previous=-1.
    if not ranges: ranges=[{'start':0,'end':duration}]
    if len(ranges)>300: raise MediaError('Há cortes demais. Divida o trabalho em partes menores.')
    for r in ranges:
        a = number(r.get('start'),0,0,duration)
        b = number(r.get('end'),duration,0,duration)
        a=round(a*fps)/fps; b=min(duration,round(b*fps)/fps)
        if b-a < 1/fps: continue
        if a < previous-0.0001: raise MediaError('Os cortes devem estar em ordem e sem sobreposição.')
        result.append({'source_start':a,'source_end':b,'output_start':cursor,'output_end':cursor+b-a})
        cursor+=b-a; previous=b
    if not result: raise MediaError('A seleção não contém nenhum quadro.')
    return {'version':1,'fps':fps,'duration':cursor,'segments':result}

def silence_ranges(stderr, duration, handle=0.16):
    silence=[]; start=None
    for kind,num in re.findall(r'silence_(start|end):\s*([\d.]+)',stderr):
        if kind=='start': start=max(0,float(num))
        else:
            end=min(duration,float(num))
            if start is None: start=0
            if end-start>handle*2: silence.append((start+handle,end-handle))
            start=None
    if start is not None and duration-start>handle*2: silence.append((start+handle,duration-handle))
    keep=[]; cursor=0
    for a,b in silence:
        if a>cursor: keep.append({'start':cursor,'end':a})
        cursor=max(cursor,b)
    if cursor<duration: keep.append({'start':cursor,'end':duration})
    return keep or [{'start':0,'end':duration}]

def plan_silence(req, meta=None):
    meta=meta or probe(req)
    if not meta['has_audio']: return {'timeline':timeline(None,meta['duration'],fps_for(meta)),'warning':'Vídeo sem áudio; nenhum corte por silêncio foi proposto.'}
    o=req.get('options',{}); noise=number(o.get('threshold_db'),-38,-80,-10)
    minimum=number(o.get('minimum_silence'),0.65,0.2,20); handle=number(o.get('handles'),0.16,0,2)
    progress('Analisando pausas')
    _,err=ffmpeg(req,['-i',meta['path'],'-vn','-af',f'silencedetect=noise={noise}dB:d={minimum}','-f','null','-'],timeout=max(120,meta['duration']*3))
    keep=silence_ranges(err,meta['duration'],handle)
    graph=timeline(keep,meta['duration'],fps_for(meta))
    return {'timeline':graph,'removed_seconds':round(meta['duration']-graph['duration'],3),
            'warning':'A detecção usa volume: revise pausas intencionais, música e fala baixa antes de exportar.',
            'settings':{'threshold_db':noise,'minimum_silence':minimum,'handles':handle}}

def speech_device():
    import ctranslate2
    try:
        supported=set(ctranslate2.get_supported_compute_types('cuda'))
        # Blackwell does not universally support INT8. Use FP16 only if reported.
        if 'float16' in supported: return 'cuda','float16',sorted(supported)
        if 'float32' in supported: return 'cuda','float32',sorted(supported)
    except Exception: pass
    supported=set(ctranslate2.get_supported_compute_types('cpu'))
    return 'cpu',('int8' if 'int8' in supported else 'float32'),sorted(supported)

def transcribe(req, meta=None):
    try:
        os.environ['HF_HUB_OFFLINE']='1'; os.environ['HF_HUB_DISABLE_TELEMETRY']='1'; os.environ['DO_NOT_TRACK']='1'
        import onnxruntime
        onnxruntime.disable_telemetry_events()
        from faster_whisper import WhisperModel
    except ImportError: raise MediaError('Transcrição não instalada. Abra Reparar instalação e ative Transcrição.')
    meta=meta or probe(req)
    if not meta['has_audio']: raise MediaError('Este arquivo não possui uma faixa de áudio para transcrever.')
    if meta['duration'] <= 0: raise MediaError('O arquivo está vazio ou não tem duração legível.')
    modelpath=req.get('whisper_model_path')
    if not modelpath or not Path(modelpath,'model.bin').is_file(): raise MediaError('Modelo de transcrição ausente. Conclua o download em Reparar instalação.')
    o=req.get('options',{}); device,compute,supported=speech_device(); notices=[]
    if device=='cpu': notices.append('Transcrição executada na CPU; o backend CUDA não está disponível nesta sessão e o processamento pode levar mais tempo.')
    def attempt(dev,ct):
        progress('Transcrevendo',0,device=dev,compute_type=ct)
        model=WhisperModel(str(modelpath),device=dev,compute_type=ct,local_files_only=True,cpu_threads=max(1,min(8,(os.cpu_count() or 4)//2)),num_workers=1)
        segments,info=model.transcribe(meta['path'],language=o.get('language') or 'pt',beam_size=3,word_timestamps=True,vad_filter=True,condition_on_previous_text=False)
        result=[]
        for s in segments:
            check_cancel(req)
            result.append({'start':s.start,'end':s.end,'text':s.text.strip(),'words':[{'start':w.start,'end':w.end,'word':w.word.strip(),'probability':w.probability} for w in (s.words or [])]})
            progress('Transcrevendo',min(.999,s.end/max(.001,meta['duration'])),device=dev)
        return result,info.language
    try: segments,language=attempt(device,compute)
    except Exception as e:
        if isinstance(e,MediaError): raise
        if device!='cuda': raise MediaError('Falha na transcrição local: '+str(e)[:1000])
        notices.append('A transcrição CUDA falhou; concluída com CPU. Revise o diagnóstico de GPU antes de reativar CUDA.')
        device,compute='cpu','int8'
        try: segments,language=attempt(device,compute)
        except Exception as second: raise MediaError('A transcrição falhou na GPU e CPU: '+str(second)[:1000])
    if not segments: notices.append('Nenhuma fala foi reconhecida. Confira o áudio e o idioma antes de tentar novamente.')
    result={'version':1,'language':language,'duration':meta['duration'],'segments':segments,'device':device,'compute_type':compute,'supported_compute_types':supported,'warnings':notices}
    path=output_dir(req)/'transcricao.json'; write_json(path,result)
    return {**result,'transcript_path':str(path)}

def validate_transcript(data, duration):
    segments=[]
    for s in data.get('segments',[]):
        a=number(s.get('start'),0,0,duration); b=number(s.get('end'),duration,0,duration)
        if b<=a: continue
        text=str(s.get('text','')).strip()[:3000]
        if not text: continue
        words=[]
        for w in s.get('words',[]):
            x=number(w.get('start'),a,a,b); y=number(w.get('end'),b,a,b)
            if y>x: words.append({'start':x,'end':y,'word':str(w.get('word','')).strip()[:100]})
        segments.append({'start':a,'end':b,'text':text,'words':words})
    return sorted(segments,key=lambda s:s['start'])

def remap_transcript(segments,graph):
    mapped=[]
    for s in segments:
        for cut in graph['segments']:
            a=max(s['start'],cut['source_start']); b=min(s['end'],cut['source_end'])
            if b<=a: continue
            shift=cut['output_start']-cut['source_start']
            words=[]
            for w in s.get('words',[]):
                x=max(w['start'],a); y=min(w['end'],b)
                if y>x: words.append({**w,'start':x+shift,'end':y+shift})
            text=' '.join(w['word'] for w in words) if s.get('words') else s['text']
            if text: mapped.append({'start':a+shift,'end':b+shift,'text':text,'words':words})
    return sorted(mapped,key=lambda s:s['start'])

def timestamp(seconds, ass=False, vtt=False):
    if ass:
        n=round(seconds*100); h,n=divmod(n,360000); m,n=divmod(n,6000); s,n=divmod(n,100)
        return f'{h}:{m:02}:{s:02}.{n:02}'
    n=round(seconds*1000); h,n=divmod(n,3600000); m,n=divmod(n,60000); s,n=divmod(n,1000)
    return f'{h:02}:{m:02}:{s:02}{"." if vtt else ","}{n:03}'

def brand_of(req): return req.get('project',{}).get('brand_snapshot') or req.get('project',{}).get('brand') or {}

def color(brand,role,fallback):
    colors=brand.get('colors') or brand.get('palette') or []
    if isinstance(colors,dict): colors=[{'role':k,'hex':v} for k,v in colors.items()]
    for c in colors:
        if isinstance(c,str): continue
        value=c.get('hex') or c.get('value') or c.get('color')
        if c.get('role')==role and re.fullmatch(r'#[0-9a-fA-F]{6}',str(value)): return value
    return fallback

def dimensions(project,meta=None,preview=False):
    meta=meta or {}; aspect=project.get('aspect') or project.get('aspect_ratio') or 'source'
    resolution=project.get('resolution')
    if resolution in ('source','preserve'):
        if not meta.get('width') or not meta.get('height'): raise MediaError('Para preservar a resolução, selecione um vídeo de origem ou escolha a resolução deste projeto.')
        short=min(meta['width'],meta['height'])
        if short>2160: raise MediaError('A origem ultrapassa o limite de 2160 pixels no lado menor. Escolha uma resolução menor para este projeto.')
    else: short=int(number(resolution,1080,240,2160))
    if preview: short=min(short,540)
    ratios={'9:16':(9,16),'16:9':(16,9),'1:1':(1,1),'4:5':(4,5)}
    if aspect=='source':
        if not meta.get('width'): raise MediaError('Escolha o formato deste projeto antes de criar a animação.')
        w,h=meta['width'],meta['height']; scale=min(1,short/min(w,h)); w*=scale; h*=scale
    elif aspect in ratios:
        a,b=ratios[aspect]; w=short*a/min(a,b); h=short*b/min(a,b)
    else: raise MediaError('Formato de vídeo inválido.')
    return max(2,round(w/2)*2),max(2,round(h/2)*2)

def caption_chunks(segments,font_path,width,font_size):
    try:
        from PIL import ImageFont
        font=ImageFont.truetype(str(font_path),font_size) if font_path else ImageFont.load_default(size=font_size)
        measure=lambda s:font.getlength(s)
    except Exception: measure=lambda s:len(s)*font_size*.58
    result=[]
    for s in segments:
        words=s.get('words')
        if not words:
            tokens=s['text'].split(); span=s['end']-s['start']
            words=[{'word':v,'start':s['start']+span*i/max(1,len(tokens)),'end':s['start']+span*(i+1)/max(1,len(tokens))} for i,v in enumerate(tokens)]
        group=[]
        def flush():
            if group: result.append({'start':group[0]['start'],'end':group[-1]['end'],'text':' '.join(w['word'] for w in group),'words':list(group)})
        for word in words:
            proposed=' '.join(w['word'] for w in [*group,word])
            if group and (measure(proposed)>width*1.65 or word['end']-group[0]['start']>4.5): flush(); group=[]
            group.append(word)
        flush()
    for c in result:
        line=''; lines=[]
        for word in c['text'].split():
            test=(line+' '+word).strip()
            if line and measure(test)>width: lines.append(line); line=word
            else: line=test
        if line: lines.append(line)
        c['text']='\n'.join(lines[:2]) if len(lines)<=2 else '\n'.join(lines)
    return result

def inspect_font(req):
    try: from fontTools.ttLib import TTFont
    except ImportError: raise MediaError('Leitor de fontes ausente. Abra Reparar instalação.')
    path=source_path(req,req['input'])
    with TTFont(str(path),lazy=True) as font:
        family=font['name'].getBestFamilyName() or path.stem
        style=font['name'].getBestSubFamilyName() or 'Regular'
        glyphs=set((font.getBestCmap() or {}).keys())
        required='ÁÀÂÃÉÊÍÓÔÕÚÜÇáàâãéêíóôõúüç'
        missing=''.join(c for c in required if ord(c) not in glyphs)
        weight=int(font['OS/2'].usWeightClass) if 'OS/2' in font else 400
        return {'family':family,'style':style,'weight':weight,'portuguese_supported':not missing,'missing_glyphs':missing}

def captions(req,transcript=None,graph=None,meta=None):
    out=output_dir(req); o=req.get('options',{}); meta=meta or probe(req)
    if transcript is None:
        if o.get('transcript_path'): transcript=read_json(source_path(req,o['transcript_path']))
        elif o.get('transcript'): transcript=o['transcript']
        else: transcript=transcribe(req,meta)
    segments=validate_transcript(transcript,meta['duration'])
    if graph: segments=remap_transcript(segments,graph)
    project=req.get('project',{}); w,h=dimensions(project,meta); brand=brand_of(req)
    font_path=None; font_family='Arial'; warnings=[]
    for f in brand.get('fonts',[]):
        if isinstance(f,dict) and f.get('path') and f.get('role') in ('caption','body',None):
            try:
                font_path=source_path(req,f['path']); metadata=inspect_font({**req,'input':str(font_path)}); font_family=metadata['family'];
                if not metadata['portuguese_supported']: warnings.append('A fonte de legenda não contém todos os acentos portugueses.')
                break
            except (MediaError,FileNotFoundError): warnings.append('Fonte cadastrada ausente; usada fonte de sistema.')
    font_size=int(number(o.get('caption_size'),round(min(w,h)*.052),16,160))
    if any(not seg.get('words') for seg in segments): warnings.append('Trechos sem timestamps por palavra usam divisão aproximada; revise as legendas e o destaque.')
    chunks=caption_chunks(segments,font_path,w*.84,font_size)
    # No overlaps; frame-quantize ends and minimum duration without crossing next cue.
    fps=fps_for(meta); total=graph['duration'] if graph else meta['duration']
    for i,c in enumerate(chunks):
        c['start']=max(0,round(c['start']*fps)/fps)
        next_start=chunks[i+1]['start'] if i+1<len(chunks) else total
        c['end']=min(total,next_start,max(c['start']+1/fps,round(c['end']*fps)/fps))
    chunks=[c for c in chunks if c['end']>c['start']]
    srt=[];vtt=['WEBVTT\n']
    for i,c in enumerate(chunks):
        srt.append(f'{i+1}\n{timestamp(c["start"])} --> {timestamp(c["end"])}\n{c["text"]}\n')
        vtt.append(f'{timestamp(c["start"],vtt=True)} --> {timestamp(c["end"],vtt=True)}\n{c["text"]}\n')
    (out/'legendas.srt').write_text('\n'.join(srt),encoding='utf-8')
    (out/'legendas.vtt').write_text('\n'.join(vtt),encoding='utf-8')
    # ASS escape all user text: never interpret arbitrary drawing or override tags.
    def esc(t): return t.replace('\\','\\\\').replace('{','｛').replace('}','｝').replace('\n','\\N')
    def asscolor(hex): return '&H00'+hex[5:7]+hex[3:5]+hex[1:3]
    accent=asscolor(color(brand,'accent','#8BDE74')); family=re.sub(r'[,\r\n]','',font_family)
    margin=int(h*(.14 if h>w else .10))
    header=f'[Script Info]\nScriptType: v4.00+\nPlayResX: {w}\nPlayResY: {h}\nWrapStyle: 0\n\n[V4+ Styles]\nFormat: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding\nStyle: Default,{family},{font_size},&H00FFFFFF,{accent},&H00141414,&H90141414,0,0,0,0,100,100,0,0,1,3,1,2,{int(w*.08)},{int(w*.08)},{margin},1\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n'
    body=[]
    for c in chunks:
        text=esc(c['text'])
        if o.get('highlight_words') and c.get('words'):
            text=' '.join('{\\kf'+str(max(1,round((word['end']-word['start'])*100)))+'}'+esc(word['word']) for word in c['words'])
        body.append(f'Dialogue: 0,{timestamp(c["start"],ass=True)},{timestamp(c["end"],ass=True)},Default,,0,0,0,,{text}')
    (out/'legendas.ass').write_text(header+'\n'.join(body)+'\n',encoding='utf-8-sig')
    write_json(out/'legendas.json',{'segments':chunks,'timeline_applied':bool(graph),'width':w,'height':h})
    if font_path:
        fontdir=out/'fonts'; fontdir.mkdir(exist_ok=True); shutil.copyfile(font_path,fontdir/font_path.name)
    return {'srt':str(out/'legendas.srt'),'vtt':str(out/'legendas.vtt'),'ass':str(out/'legendas.ass'),'captions_json':str(out/'legendas.json'),'count':len(chunks),'warnings':warnings}

def ff_filter_path(path):
    # FFmpeg filter-parser escaping (separate from shell escaping; no shell is used).
    return str(path).replace('\\','/').replace(':','\\:').replace("'","'\\''").replace('[','\\[').replace(']','\\]').replace(',','\\,')

def audio_config(project):
    a=project.get('audio')
    if isinstance(a,str): a={'mode':a}
    a=a or {}; mode=a.get('mode') or 'original'
    aliases={'mute':'none','sem_audio':'none','soundtrack':'music','original_audio':'original'}
    mode=aliases.get(mode,mode)
    if mode not in ('original','none','music','narration','mix'): raise MediaError('Escolha uma opção de áudio válida para este projeto.')
    return {**a,'mode':mode}

def edit(req):
    meta=probe(req); out=output_dir(req); o=req.get('options',{}); project=req.get('project',{}); notices=[]
    if not meta['has_video']: raise MediaError('Para editar vídeo, selecione um arquivo com imagem.')
    graph=timeline(o.get('ranges'),meta['duration'],fps_for(meta))
    if o.get('remove_silence'):
        if o.get('ranges'): raise MediaError('Aplique os cortes manuais ou a remoção de pausas em revisões separadas.')
        plan=plan_silence(req,meta); graph=plan['timeline']; notices.append(plan['warning'])
    # Requested duration truncates the assembled timeline (never loops footage silently).
    d=project.get('duration')
    if d in ('suggest','custom','propose'): raise MediaError('Defina a duração deste vídeo antes de exportar: informe os segundos ou escolha preservar a origem.')
    if d not in (None,'','source','preserve','propose'):
        target=number(d,graph['duration'],0.1,86400); left=target; trimmed=[]
        for c in graph['segments']:
            length=min(left,c['source_end']-c['source_start'])
            if length<=0: break
            trimmed.append({'start':c['source_start'],'end':c['source_start']+length}); left-=length
        graph=timeline(trimmed,meta['duration'],fps_for(meta))
        if target>graph['duration']+.1: notices.append('A duração exportada respeita a mídia disponível; não foram criados quadros extras.')
    graph.update(source=meta['path'],source_sha256=digest(meta['path']),brand_snapshot=brand_of(req))
    write_json(out/'timeline.json',graph)
    if shutil.disk_usage(out).free < max(64*1024*1024,graph['duration']*4*1024*1024): raise MediaError('Espaço livre insuficiente para a exportação e arquivos temporários.')
    cap=None
    if o.get('captions') or o.get('burn_captions'):
        cap=captions(req,graph=graph,meta=meta); notices.extend(cap['warnings']); (out/'fonts').mkdir(exist_ok=True)
    width,height=dimensions(project,meta,o.get('preview',False)); fps=fps_for(meta)
    ac=audio_config(project); original=meta['has_audio'] and ac['mode'] in ('original','mix')
    args=['-i',meta['path']]; extras=[]
    for key in ('music','narration'):
        if ac['mode'] in (key,'mix') and ac.get(key+'_path'):
            p=source_path(req,ac[key+'_path']); args+=['-i',str(p)]; extras.append((key,len(extras)+1))
        elif ac['mode']==key: raise MediaError('Adicione o arquivo de '+('música' if key=='music' else 'narração')+' deste projeto.')
    filters=[]; labels=[]
    for i,c in enumerate(graph['segments']):
        a,b=c['source_start'],c['source_end']
        filters.append(f'[0:v:0]trim=start={a:.8f}:end={b:.8f},setpts=PTS-STARTPTS[v{i}]')
        if original: filters.append(f'[0:a:0]atrim=start={a:.8f}:end={b:.8f},asetpts=PTS-STARTPTS[a{i}]')
        labels.extend([f'[v{i}]']+([f'[a{i}]'] if original else []))
    n=len(graph['segments']); filters.append(''.join(labels)+f'concat=n={n}:v=1:a={1 if original else 0}[vcat]'+('[acat]' if original else ''))
    fit=o.get('fit','contain')
    if fit not in ('contain','cover'): raise MediaError('Enquadramento inválido.')
    vf=f'scale={width}:{height}:force_original_aspect_ratio='+('increase,crop='+str(width)+':'+str(height) if fit=='cover' else f'decrease,pad={width}:{height}:(ow-iw)/2:(oh-ih)/2:color=black')
    if meta['hdr']:
        vf='zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,'+vf
        notices.append('Conversão HDR para SDR aplicada com tonemap Hable; revise a cor da prévia.')
    vf+=f',setsar=1,fps={fps:.8f},format=yuv420p'
    if cap and o.get('burn_captions',True): vf+=",subtitles=filename='legendas.ass':fontsdir='fonts'"
    filters.append('[vcat]'+vf+'[vout]')
    alabels=[]
    if original:
        level=number(ac.get('original_level'),1,0,3); af=f'volume={level}'
        if o.get('improve_audio'): af+=',highpass=f=70,afftdn=nf=-25,loudnorm=I=-16:TP=-1.5:LRA=11'
        filters.append('[acat]'+af+'[aoriginal]'); alabels.append('[aoriginal]')
    for kind,index in extras:
        level=number(ac.get(kind+'_level'),.18 if kind=='music' else 1,0,3)
        filters.append(f'[{index}:a:0]atrim=duration={graph["duration"]},asetpts=PTS-STARTPTS,volume={level},apad=whole_dur={graph["duration"]}[a{kind}]'); alabels.append(f'[a{kind}]')
    if alabels:
        filters.append(''.join(alabels)+f'amix=inputs={len(alabels)}:duration=longest:normalize=0,alimiter=limit=0.84,atrim=duration={graph["duration"]}[aout]')
    write_json(out/'export-plan.json',{'width':width,'height':height,'fps':fps,'audio':ac,'options':o,'warnings':notices})
    (out/'filters.txt').write_text(';\n'.join(filters),encoding='utf-8')
    target=out/('previa.mp4' if o.get('preview') else 'video.mp4')
    base=[*args,'-filter_complex_script',str(out/'filters.txt'),'-map','[vout]']
    if alabels: base+=['-map','[aout]','-c:a','aac','-b:a','192k']
    else: base+=['-an']
    encoding=['-c:v','h264_nvenc','-preset','p4','-cq','22'] if o.get('use_nvenc') else ['-c:v','libx264','-preset','veryfast','-crf','21']
    tail=['-t',str(graph['duration']),'-pix_fmt','yuv420p','-movflags','+faststart','-progress','pipe:1','-nostats',str(target)]
    progress('Exportando',0)
    try: ffmpeg(req,[*base,*encoding,*tail],stage='Exportando',duration=graph['duration'],timeout=max(300,graph['duration']*40),cwd=out)
    except MediaError:
        if not o.get('use_nvenc'): raise
        check_cancel(req); notices.append('NVENC indisponível; exportação realizada com CPU.')
        ffmpeg(req,[*base,'-c:v','libx264','-preset','veryfast','-crf','21',*tail],stage='Exportando com CPU',duration=graph['duration'],timeout=max(300,graph['duration']*50),cwd=out)
    validation=probe({**req,'allowed_roots':[str(out)]},target)
    if abs(validation['duration']-graph['duration']) > max(.2,2/fps): raise MediaError('A duração exportada diverge da timeline. O arquivo foi preservado para diagnóstico.')
    if validation['has_audio'] != bool(alabels): raise MediaError('A faixa de áudio exportada não corresponde à seleção.')
    ffmpeg(req,['-ss',str(min(graph['duration']/2,2)),'-i',str(target),'-frames:v','1','-vf','scale=640:-2',str(out/'previa.jpg')],timeout=60)
    if digest(meta['path'])!=graph['source_sha256']: raise MediaError('O arquivo original mudou durante o trabalho; confira a origem antes de usar o resultado.')
    return {'output':str(target),'preview_image':str(out/'previa.jpg'),'timeline':str(out/'timeline.json'),'captions':cap,'probe':validation,'warnings':notices,'original_unchanged':True}

def frames(req):
    meta=probe(req); out=output_dir(req)
    if not meta['has_video']: raise MediaError('O arquivo não contém imagem.')
    count=int(number(req.get('options',{}).get('count'),6,1,20)); result=[]
    for i in range(count):
        moment=meta['duration']*(i+.5)/count
        target=out/f'quadro-{i+1:02}.jpg'
        ffmpeg(req,['-ss',str(moment),'-i',meta['path'],'-frames:v','1','-vf','scale=640:-2',str(target)],timeout=90)
        result.append({'time':moment,'path':str(target)}); progress('Lendo quadros',(i+1)/count)
    return {'frames':result,'sampling_notice':f'{count} quadros amostrados. A amostragem não representa todos os movimentos do vídeo.'}

def motion(req):
    out=output_dir(req); project=req.get('project',{}); o=req.get('options',{}); brand=brand_of(req)
    remotion=Path(req.get('remotion_dir') or Path(__file__).parent.parent/'remotion').resolve()
    if not (remotion/'node_modules'/'remotion').exists():
        progress('Instalando motor do Remotion')
        try:
            node_exe=binary(req,'node')
            node_dir=Path(node_exe).parent
            npm_cli=node_dir/'node_modules'/'npm'/'bin'/'npm-cli.js'
            if npm_cli.exists():
                run(req,[node_exe,str(npm_cli),'ci','--no-audit','--no-fund'],stage='Instalando Remotion',duration=None,timeout=300,cwd=remotion)
        except Exception: pass
    if not (remotion/'node_modules'/'remotion').exists(): raise MediaError('Motor de animação não instalado. Abra Reparar instalação.')
    assets=out/'motion-assets'; assets.mkdir(exist_ok=True); fonts=[]; logos=[]; notices=[]
    def copy_asset(path):
        p=source_path(req,path)
        if p.suffix.lower()=='.svg':
            svg=p.read_text(encoding='utf-8-sig')
            if re.search(r'<script|<!DOCTYPE|<!ENTITY|\bon\w+\s*=|(?:href|src)\s*=\s*[\"\'](?!#)',svg,re.I): raise MediaError('SVG contém conteúdo ativo ou externo. Importe uma versão PNG.')
        name=hashlib.sha256(str(p).encode()).hexdigest()[:12]+p.suffix.lower(); shutil.copyfile(p,assets/name); return name
    for f in brand.get('fonts',[]):
        if isinstance(f,dict) and f.get('path'):
            try:
                metadata=inspect_font({**req,'input':f['path']})
                fonts.append({**f,'src':copy_asset(f['path']),'family':metadata['family'],'weight':str(metadata['weight'])})
                if not metadata['portuguese_supported']: notices.append('A fonte '+metadata['family']+' não contém todos os acentos portugueses.')
            except (MediaError,FileNotFoundError): notices.append('Fonte de marca indisponível; usando fallback local.')
    for logo in brand.get('logos',[]):
        p=logo.get('path') if isinstance(logo,dict) else logo
        try: logos.append(copy_asset(p))
        except (MediaError,FileNotFoundError): notices.append('Logo cadastrado indisponível; revise a identidade antes de aprovar.')
    source_meta=probe(req) if req.get('input') else None
    if source_meta and not source_meta['has_video']: raise MediaError('A animação de fundo precisa de um arquivo com imagem.')
    if source_meta and source_meta['hdr']: raise MediaError('Converta a mídia HDR para SDR em Ajustar formato antes de sobrepor animações.')
    width,height=dimensions(project,source_meta,preview=o.get('preview',False))
    duration=project.get('duration')
    if duration in ('suggest','custom'): raise MediaError('Defina a duração deste vídeo: informe os segundos ou escolha preservar a origem quando houver vídeo.')
    if duration in ('source','preserve') and source_meta: duration=source_meta['duration']
    if duration in (None,'','source','preserve','propose') and req['action']!='carousel': raise MediaError('Defina a duração deste vídeo para gerar a animação.')
    duration=number(duration,5,1,300) if req['action']!='carousel' else 5
    scenes=o.get('scenes') or project.get('scenes')
    if not scenes:
        title=o.get('title') or project.get('title')
        if not title and project.get('briefing'):
            raw_b=project.get('briefing','').strip()
            lines=[l.strip() for l in raw_b.splitlines() if l.strip() and not l.strip().startswith('#') and not l.strip().startswith('*')]
            title=(lines[0] if lines else raw_b)[:100]
        if not title:
            title=brand.get('name') or 'Apresentação'
        scenes=[{'type':'kinetic','title':title,'body':o.get('body','') or (project.get('briefing','')[:250] if not o.get('body') else ''),'duration':duration}]
    if not isinstance(scenes,list) or len(scenes)>40: raise MediaError('Use no máximo 40 cenas.')
    allowed={'title','lower-third','logo','kinetic','callout','card','explainer'}
    clean=[]
    for s in scenes:
        typ=s.get('type','title')
        if typ not in allowed: raise MediaError('Tipo de cena não disponível.')
        clean.append({'type':typ,'title':str(s.get('title',''))[:180],'body':str(s.get('body',''))[:650], 'eyebrow':str(s.get('eyebrow',''))[:80], 'duration':number(s.get('duration'),duration/len(scenes),.5,300)})
    if duration < len(clean)*.5: raise MediaError('A duração escolhida é curta para a quantidade de cenas. Use pelo menos meio segundo por cena.')
    total=sum(s['duration'] for s in clean)
    for s in clean: s['duration']*=duration/total
    audio=audio_config(project); audio_assets=[]; background_video=None; original_volume=0
    if source_meta:
        if duration>source_meta['duration']+.1: raise MediaError('A duração solicitada ultrapassa o vídeo de fundo. Reduza a duração ou use uma animação sem esse vídeo.')
        if shutil.disk_usage(out).free < Path(source_meta['path']).stat().st_size+128*1024*1024: raise MediaError('Sem espaço para preparar a mídia da animação.')
        background_video=copy_asset(source_meta['path'])
        if source_meta['has_audio'] and audio['mode'] in ('original','mix'): original_volume=number(audio.get('original_level'),1,0,3)
    for kind in ('music','narration'):
        if audio['mode'] in (kind,'mix'):
            if audio.get(kind+'_path'): audio_assets.append({'src':copy_asset(audio[kind+'_path']),'volume':number(audio.get(kind+'_level'),.18 if kind=='music' else 1,0,3)})
            elif audio['mode']==kind: raise MediaError('Adicione o arquivo de áudio escolhido para este vídeo.')
    if audio['mode']=='original' and not source_meta: notices.append('Animação sem mídia de origem: não existe áudio original para preservar.')
    payload={'width':width,'height':height,'fps':30,'duration':duration,'scenes':clean,'brand':{'name':str(brand.get('name',''))[:100],'background':color(brand,'background','#11191E'),'text':color(brand,'text','#F5F6F2'),'accent':color(brand,'accent',color(brand,'primary','#A9EE7E'))},'fonts':fonts,'logos':logos,'audio':audio_assets,'backgroundVideo':background_video,'originalVolume':original_volume,'fit':'cover' if o.get('fit')=='cover' else 'contain','assets_dir':str(assets),'output_dir':str(out),'mode':req['action'],'preview_only':bool(o.get('preview_only'))}
    write_json(out/'motion.json',payload)
    progress('Preparando animação')
    stdout,_=run(req,[binary(req,'node'),remotion/'render.mjs','--request',out/'motion.json'],stage='Renderizando',duration=None,timeout=max(600,duration*60),cwd=remotion)
    result=None
    for line in stdout.splitlines():
        try:
            event=json.loads(line)
            if event.get('type')=='result': result=event
        except ValueError: pass
    if not result: raise MediaError('O motor de animação terminou sem confirmar o resultado.')
    if result.get('output') and req.get('ffprobe'):
        rendered=probe({**req,'allowed_roots':[str(out)]},result['output'])
        if abs(rendered['duration']-duration)>.2 or rendered['width']!=width or rendered['height']!=height: raise MediaError('A animação exportada diverge do formato ou duração escolhidos.')
        expected_audio=bool(original_volume or audio_assets)
        if rendered['has_audio']!=expected_audio: raise MediaError('A animação exportada não corresponde à escolha de áudio.')
        result['probe']=rendered
    return {**{k:v for k,v in result.items() if k!='type'},'warnings':notices,'motion_project':str(out/'motion.json')}

def diagnose(req):
    results={}; out=output_dir(req)
    for name in ('ffmpeg','ffprobe','node'):
        try:
            stdout,err=run(req,[binary(req,name),'-version' if name!='node' else '--version'],timeout=30)
            results[name]={'ok':True,'version':(stdout or err).splitlines()[0]}
        except Exception as e: results[name]={'ok':False,'error':str(e)}
    if results['ffmpeg']['ok']:
        try:
            stdout,err=ffmpeg(req,['-filters'],timeout=30)
            text=stdout+err; required=['subtitles','silencedetect','loudnorm','concat','trim','atrim','zscale','tonemap']
            results['filters']={k:bool(re.search(r'\b'+k+r'\b',text)) for k in required}
            target=out/'nvenc-check.mp4'
            ffmpeg(req,['-f','lavfi','-i','color=c=black:s=320x240:d=0.25','-c:v','h264_nvenc','-an',str(target)],timeout=60)
            results['nvenc']={'ok':target.exists() and target.stat().st_size>0}
        except Exception as e: results['nvenc']={'ok':False,'fallback':'libx264','detail':str(e)[-700:]}
    try:
        device,compute,supported=speech_device();results['speech']={'device':device,'compute_type':compute,'supported':supported,'transcription_tested':False}
    except Exception as e: results['speech']={'ok':False,'detail':str(e)}
    return {'diagnostics':results,'notice':'Detecção de CUDA não substitui uma transcrição real no computador de destino.'}

def extract_document(req):
    path=source_path(req,req['input']); suffix=path.suffix.lower(); chunks=[]; warnings=[]
    if path.stat().st_size>100*1024*1024: raise MediaError('Documento maior que 100 MB. Importe uma versão menor.')
    if suffix=='.pdf':
        try: from pypdf import PdfReader
        except ImportError: raise MediaError('Leitor PDF indisponível. Abra Reparar instalação.')
        reader=PdfReader(str(path))
        if reader.is_encrypted: raise MediaError('PDF protegido: importe uma cópia autorizada sem senha.')
        if len(reader.pages)>1000: raise MediaError('PDF com mais de 1000 páginas. Importe em partes.')
        for index,page in enumerate(reader.pages):
            check_cancel(req); text=page.extract_text() or ''
            if text.strip(): chunks.append({'text':text[:500000],'page':index+1})
            else: warnings.append(f'Página {index+1} sem texto extraível. OCR não está instalado neste pacote.')
            progress('Lendo documento',(index+1)/max(1,len(reader.pages)))
    elif suffix=='.docx':
        try: from docx import Document
        except ImportError: raise MediaError('Leitor DOCX indisponível. Abra Reparar instalação.')
        document=Document(str(path)); lines=[p.text for p in document.paragraphs if p.text.strip()]
        for table in document.tables:
            for row in table.rows: lines.append(' | '.join(cell.text for cell in row.cells))
        text='\n'.join(lines)
        for i in range(0,len(text),6000): chunks.append({'text':text[i:i+6000],'page':0,'section':i//6000+1})
    else: raise MediaError('Formato documental não suportado por este extrator.')
    return {'chunks':chunks,'warnings':warnings,'status':'ready' if chunks else 'failed','sha256':digest(path)}

def main():
    parser=argparse.ArgumentParser(); parser.add_argument('--request',required=True); args=parser.parse_args()
    try:
        req=read_json(args.request)
        if not isinstance(req,dict): raise MediaError('Pedido inválido.')
        action=req.get('action'); functions={'probe':probe,'frames':frames,'transcribe':transcribe,'plan_silence':plan_silence,'captions':captions,'edit':edit,'motion':motion,'carousel':motion,'diagnose':diagnose,'extract_document':extract_document,'inspect_font':inspect_font}
        if action not in functions: raise MediaError('Operação não disponível.')
        output_dir(req); progress('Iniciando')
        result=functions[action](req)
        if action=='plan_silence': result['plan_path']=write_json(output_dir(req)/'plano-cortes.json',result)
        emit('result',**result)
    except Exception as e:
        emit('error',message=str(e)[:4500],recoverable=True); sys.exit(1)

if __name__=='__main__': main()
