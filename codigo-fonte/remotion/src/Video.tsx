import React, {useEffect, useState} from 'react';
import {AbsoluteFill, Audio, Img, OffthreadVideo, Sequence, continueRender, delayRender, interpolate, spring, staticFile, useCurrentFrame, useVideoConfig} from 'remotion';
import {loadFont} from '@remotion/fonts';
import {fitText} from '@remotion/layout-utils';

export type Scene = {type:'title'|'lower-third'|'logo'|'kinetic'|'callout'|'card'|'explainer'; title:string;body:string;eyebrow?:string;duration:number};
export type MotionProps = {width:number;height:number;fps:number;duration:number;scenes:Scene[];brand:{name:string;background:string;text:string;accent:string};fonts:Array<{src:string;family:string;weight?:string;role?:string}>;logos:string[];audio:Array<{src:string;volume:number}>;assetBase?:string;carouselIndex?:number;backgroundVideo?:string;originalVolume?:number;fit?:'contain'|'cover'};
const src = (p:MotionProps,name:string) => p.assetBase ? `${p.assetBase}/${encodeURIComponent(name)}` : staticFile(name);
const clamp = {extrapolateLeft:'clamp',extrapolateRight:'clamp'} as const;

const SceneView:React.FC<{scene:Scene;props:MotionProps;index:number;frames:number}> = ({scene,props:p,index,frames}) => {
  const frame=useCurrentFrame();const {fps,width,height}=useVideoConfig();
  const min=Math.min(width,height),pad=min*.085;
  const family=p.fonts.find(f=>f.role==='heading')?.family || p.fonts[0]?.family || 'Arial, sans-serif';
  const bodyFamily=p.fonts.find(f=>f.role==='body')?.family || family;
  const fitted=scene.title ? fitText({text:scene.title,fontFamily:family,fontWeight:700,withinWidth:width-pad*2,validateFontIsLoaded:false}).fontSize : min*.10;
  const fontSize=Math.min(min*.10,Math.max(min*.052,Number.isFinite(fitted)?fitted:min*.10));
  const entrance=spring({fps,frame,config:{damping:22,stiffness:115}});
  const leave=interpolate(frame,[Math.max(1,frames-8),frames],[1,0],clamp);
  const logo=p.logos[0];const isLower=scene.type==='lower-third';const isLogo=scene.type==='logo';
  const words=scene.title.split(/\s+/);
  return <AbsoluteFill style={{background:p.backgroundVideo?'rgba(10,14,18,0.42)':p.brand.background,color:p.brand.text,fontFamily:family,padding:pad,overflow:'hidden'}}>
    <div style={{position:'absolute',left:pad,top:pad,width:min*.075,height:6,background:p.brand.accent,opacity:entrance}} />
    <div style={{position:'absolute',right:pad,top:pad,fontSize:min*.019,letterSpacing:min*.003,textTransform:'uppercase',opacity:.65}}>{p.brand.name}</div>
    {logo && !isLogo ? <Img src={src(p,logo)} style={{position:'absolute',left:pad,top:pad+min*.05,width:min*.19,height:min*.09,objectFit:'contain',objectPosition:'left center'}} /> : null}
    <div style={{position:'absolute',inset:pad,display:'flex',flexDirection:'column',justifyContent:isLower?'flex-end':'center',alignItems:isLogo?'center':'flex-start',gap:min*.027,opacity:leave,transform:`translateY(${(1-entrance)*min*.065}px)`}}>
      {scene.eyebrow ? <div style={{fontSize:min*.023,textTransform:'uppercase',letterSpacing:min*.003,color:p.brand.accent}}>{scene.eyebrow}</div> : null}
      {isLogo && logo ? <Img src={src(p,logo)} style={{width:min*.52,height:min*.27,objectFit:'contain',marginBottom:min*.05,transform:`scale(${.92+.08*entrance})`}} /> : null}
      {scene.type==='callout' ? <div style={{fontSize:min*.12,color:p.brand.accent,lineHeight:1}}>“</div> : null}
      {scene.type==='kinetic' ? <div style={{fontSize,lineHeight:1.06,fontWeight:700,letterSpacing:-min*.002,maxWidth:'100%'}}>{words.map((word,i)=><span key={i} style={{display:'inline-block',marginRight:min*.016,opacity:interpolate(frame,[i*3,i*3+5],[0,1],clamp),color:i===words.length-1?p.brand.accent:p.brand.text,transform:`translateY(${interpolate(frame,[i*3,i*3+8],[min*.04,0],clamp)}px)`}}>{word}</span>)}</div> : <div style={{fontSize:isLower?fontSize*.72:fontSize,lineHeight:1.08,fontWeight:700,letterSpacing:-min*.002,maxWidth:'100%',overflowWrap:'break-word',textAlign:isLogo?'center':'left'}}>{scene.title}</div>}
      {scene.body ? <div style={{fontFamily:bodyFamily,fontSize:Math.min(min*.036,min*.036*(240/Math.max(240,scene.body.length))**.35),lineHeight:1.4,maxWidth:width-pad*2,opacity:.85,whiteSpace:'pre-wrap',overflowWrap:'break-word',textAlign:isLogo?'center':'left'}}>{scene.body}</div> : null}
      {scene.type==='card'||scene.type==='explainer' ? <div style={{height:4,width:min*.18,background:p.brand.accent,marginTop:min*.035,transformOrigin:'left',transform:`scaleX(${entrance})`}} /> : null}
    </div>
    <div style={{position:'absolute',bottom:pad*.65,left:pad,right:pad,display:'flex',justifyContent:'space-between',fontSize:min*.017,opacity:.45}}><span>{String(index+1).padStart(2,'0')}</span><span>{p.scenes.length>1?`${index+1} / ${p.scenes.length}`:''}</span></div>
  </AbsoluteFill>;
};

export const Video:React.FC<MotionProps> = p => {
  const [handle]=useState(()=>delayRender('Carregando fontes locais'));
  const [error,setError]=useState<string|null>(null);
  const [fontsReady,setFontsReady]=useState(p.fonts.length===0);
  useEffect(()=>{
    let stopped=false;
    Promise.all(p.fonts.map(f=>loadFont({family:f.family,url:src(p,f.src),weight:f.weight||'400'})))
      .then(()=>{if(!stopped)setFontsReady(true);})
      .catch(e=>{if(!stopped)setError(String(e));});
    return ()=>{stopped=true;};
  },[handle,p.fonts,p.assetBase]);
  useEffect(()=>{if(fontsReady||error)continueRender(handle);},[fontsReady,error,handle]);
  if(error)throw new Error(`Fonte de marca não carregou: ${error}`);
  const {fps}=useVideoConfig();let cursor=0;
  if(p.carouselIndex!==undefined){const scene=p.scenes[p.carouselIndex];return <SceneView scene={scene} props={p} index={p.carouselIndex} frames={Math.round(p.duration*fps)}/>;}
  return <AbsoluteFill style={{background:p.brand.background}}>
    {p.backgroundVideo ? <OffthreadVideo src={src(p,p.backgroundVideo)} muted={!p.originalVolume} volume={p.originalVolume||0} style={{width:'100%',height:'100%',objectFit:p.fit||'contain'}}/> : null}
    {p.scenes.map((scene,index)=>{const from=cursor;const frames=index===p.scenes.length-1?Math.round(p.duration*fps)-cursor:Math.max(1,Math.round(scene.duration*fps));cursor+=frames;return <Sequence key={index} from={from} durationInFrames={Math.max(1,frames)} premountFor={fps}><SceneView scene={scene} props={p} index={index} frames={frames}/></Sequence>;})}
    {p.audio.map((a,i)=><Audio key={i} src={src(p,a.src)} volume={a.volume}/>)}
  </AbsoluteFill>;
};
