import fs from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {bundle} from '@remotion/bundler';
import {selectComposition,renderMedia,renderStill,makeCancelSignal} from '@remotion/renderer';
const root=path.dirname(fileURLToPath(import.meta.url));
const emit=o=>process.stdout.write(JSON.stringify(o)+'\n');
const index=process.argv.indexOf('--request');
if(index<0)throw new Error('Missing --request');
const p=JSON.parse(fs.readFileSync(process.argv[index+1],'utf8'));
if(!p.output_dir||!p.assets_dir)throw new Error('Missing managed paths');
if(!Number.isFinite(p.duration)||p.duration<1||p.duration>300)throw new Error('Invalid duration');
if(!Array.isArray(p.scenes)||!p.scenes.length||p.scenes.length>40)throw new Error('Invalid scenes');
const out=path.resolve(p.output_dir);fs.mkdirSync(out,{recursive:true});
const {cancelSignal,cancel}=makeCancelSignal();
process.on('SIGTERM',()=>{cancel();process.exitCode=130;});
const serveUrl=await bundle({entryPoint:path.join(root,'src/index.tsx'),publicDir:path.resolve(p.assets_dir),onProgress:value=>emit({type:'progress',stage:'Preparando prévia',progress:value/100})});
const inputProps={width:p.width,height:p.height,fps:p.fps,duration:p.duration,scenes:p.scenes,brand:p.brand,fonts:p.fonts,logos:p.logos,audio:p.audio,backgroundVideo:p.backgroundVideo,originalVolume:p.originalVolume,fit:p.fit};
const browserExecutable=process.env.REMOTION_BROWSER_EXECUTABLE||undefined;
const composition=await selectComposition({serveUrl,id:'Video',inputProps,browserExecutable,timeoutInMilliseconds:60000});
const preview=path.join(out,'motion-previa.png');
await renderStill({composition,serveUrl,inputProps,output:preview,frame:Math.min(composition.durationInFrames-1,Math.round(p.fps*1.2)),imageFormat:'png',browserExecutable,timeoutInMilliseconds:60000});
if(p.mode==='carousel'){
  const images=[];
  for(let i=0;i<p.scenes.length;i++){
    const output=path.join(out,`slide-${String(i+1).padStart(2,'0')}.png`);
    await renderStill({composition,serveUrl,inputProps:{...inputProps,carouselIndex:i},output,frame:Math.round(p.fps*1.2),imageFormat:'png',browserExecutable,timeoutInMilliseconds:60000});
    images.push(output);emit({type:'progress',stage:'Exportando carrossel',progress:(i+1)/p.scenes.length});
  }
  emit({type:'result',images,preview_image:preview});
}else if(p.preview_only){emit({type:'result',preview_image:preview});}
else {
  const output=path.join(out,'motion.mp4');
  await renderMedia({composition,serveUrl,inputProps,outputLocation:output,codec:'h264',audioCodec:'aac',crf:20,pixelFormat:'yuv420p',concurrency:2,browserExecutable,timeoutInMilliseconds:60000,cancelSignal,onProgress:({progress})=>emit({type:'progress',stage:'Renderizando animação',progress})});
  emit({type:'result',output,preview_image:preview});
}
