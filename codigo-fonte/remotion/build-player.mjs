import {build} from 'esbuild';
import fs from 'node:fs';
await build({entryPoints:['player.tsx'],bundle:true,format:'esm',minify:true,outfile:'dist/player.bundle.js',define:{'process.env.NODE_ENV':'"production"'}});
fs.copyFileSync('player.html','dist/player.html');
