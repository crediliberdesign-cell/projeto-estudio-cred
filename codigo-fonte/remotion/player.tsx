import React from 'react';
import {createRoot} from 'react-dom/client';
import {Player} from '@remotion/player';
import {Video,MotionProps} from './src/Video';
const root=createRoot(document.getElementById('root')!);
window.addEventListener('message',event=>{
  if(event.origin!==location.origin||event.data?.type!=='motion-preview')return;
  const p:MotionProps=event.data.props;
  if(!p||!Array.isArray(p.scenes)||!Number.isFinite(p.duration))return;
  root.render(<Player component={Video} inputProps={p} durationInFrames={Math.round(p.duration*p.fps)} fps={p.fps} compositionWidth={p.width} compositionHeight={p.height} controls style={{width:'100%',height:'100%'}}/>);
});
window.parent.postMessage({type:'motion-player-ready'},location.origin);
