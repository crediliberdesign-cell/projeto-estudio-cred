import React from 'react';
import {Composition, registerRoot} from 'remotion';
import {Video} from './Video';
const Root=()=> <Composition id="Video" component={Video} durationInFrames={150} width={1080} height={1920} fps={30}
  defaultProps={{width:1080,height:1920,fps:30,duration:5,scenes:[{type:'title',title:'Prévia da identidade',body:'Este exemplo não define formato, duração ou áudio dos próximos projetos.',duration:5}],brand:{name:'DEMONSTRAÇÃO',background:'#11191E',text:'#F5F6F2',accent:'#A9EE7E'},fonts:[],logos:[],audio:[]}}
  calculateMetadata={({props})=>({durationInFrames:Math.max(1,Math.round(props.duration*props.fps)),width:props.width,height:props.height,fps:props.fps})}/>;
registerRoot(Root);
