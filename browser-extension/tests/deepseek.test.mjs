import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
async function capability(name,environment,input={}){const source=await readFile(new URL('../../capabilities/deepseek/'+name+'.js',import.meta.url),'utf8');return await vm.runInNewContext('('+source+')',environment)(input);}
const input=(turn,prompt='fixture',mode='new')=>({turn_id:turn,prompt,mode});
function page({existing=false,draft='',disabled=false}={}){
 let value=draft,busy=false,sends=0,stops=0,sequence=0;
 const messages=[],banners=[],controls=[];
 const make=(kind,text='')=>{const node={kind,text,order:++sequence,querySelector(selector){if(selector==='.ds-collapsible-text')return kind==='user'?{textContent:node.text}:null;if(selector==='.ds-assistant-message-main-content')return kind==='assistant'&&node.text?{innerText:node.text}:null;return null;},compareDocumentPosition(other){return node.order<other.order?4:2;}};messages.push(node);return node;};
 if(existing){make('user','old question');make('assistant','old answer');}
 class Textarea{get value(){return value}set value(next){value=next}dispatchEvent(){return true}}
 const editor=new Textarea();
 const button={classList:{contains:()=>disabled||!value&&!busy},getAttribute:()=>null,click(){if(busy){stops++;busy=false;}else{sends++;make('user',value);make('assistant');value='';busy=true;}}};
 const stopPath={closest:()=>button};
 const composer={querySelector(selector){if(selector==='.ds-button--primary.ds-button--circle')return button;if(selector.includes('M2 4.88'))return busy?stopPath:null;return null;}};editor.parentElement=composer;
 const document={querySelector(selector){if(selector.startsWith('textarea'))return editor;if(selector==='.ds-message')return messages[0]||null;return null;},querySelectorAll(selector){if(selector==='.ds-message')return messages;if(selector==='div > span:first-child')return banners;if(selector==='[role="button"],button')return controls;return [];}};
 const environment={document,location:{origin:'https://chat.deepseek.com',pathname:'/'},HTMLTextAreaElement:Textarea,Event,Date,setTimeout,Node:{DOCUMENT_POSITION_FOLLOWING:4}};
 return {environment,enable(){disabled=false},counts:()=>({sends,stops,busy,value}),finish(text){const node=[...messages].reverse().find(node=>node.kind==='assistant');node.text=text;busy=false;},error({quoted=false}={}){const user=[...messages].reverse().find(node=>node.kind==='user');const banner={children:[],textContent:'服务器暂时不可用',order:user.order+.25,closest:()=>quoted?user:null,nextElementSibling:{querySelector:()=>({})}};banners.push(banner);busy=false;return banner;},continuation(){const user=[...messages].reverse().find(node=>node.kind==='user');controls.push({textContent:'继续生成',order:user.order+2,closest:()=>null});busy=false;}};
}

test('readiness never imports an existing user conversation',async()=>{
 assert.deepEqual(JSON.parse(JSON.stringify(await capability('read',page().environment))),{state:'idle',text:'',turn_id:''});
 assert.equal((await capability('read',page({existing:true}).environment)).state,'unsupported');
});
test('start sends exactly once with explicit mode/turn and refuses old chats or drafts',async()=>{
 const f=page();assert.equal((await capability('start',f.environment,input('turn-one'))).state,'generating');assert.equal(f.counts().sends,1);
 await assert.rejects(capability('start',f.environment,input('turn-one')),/already started/);assert.equal(f.counts().sends,1);
 for(const config of [{existing:true},{draft:'unsent personal draft'}]){const f=page(config);await assert.rejects(capability('start',f.environment,input('turn-one')),/empty conversation|existing draft/);assert.equal(f.counts().sends,0);}
 await assert.rejects(capability('start',page().environment,input('turn-two','next','continue')),/runtime-owned/);
});
test('continue stays in one owned page and never reuses the previous assistant answer as completion',async()=>{
 const f=page();await capability('start',f.environment,input('turn-one','first'));f.finish('first answer');assert.equal((await capability('read',f.environment,{turn_id:'turn-one'})).text,'first answer');
 const started=await capability('start',f.environment,input('turn-two','only new messages','continue'));assert.equal(started.turn_id,'turn-two');assert.equal(started.text,'');assert.equal(started.state,'generating');
 const pending=await capability('read',f.environment,{turn_id:'turn-two'});assert.equal(pending.text,'');assert.notEqual(pending.state,'complete');
 f.finish('second answer');const done=await capability('read',f.environment,{turn_id:'turn-two'});assert.equal(done.state,'complete');assert.equal(done.text,'second answer');assert.equal(f.counts().sends,2);
 await assert.rejects(capability('read',f.environment,{turn_id:'turn-one'}),/identity changed/);
});
test('an explicit retry can replace only its own prepared unsent draft',async()=>{
 const f=page({disabled:true});assert.equal((await capability('start',f.environment,input('turn-one','prepared'))).state,'not_ready');assert.equal(f.counts().sends,0);
 assert.equal((await capability('read',f.environment,{turn_id:'turn-one'})).state,'not_ready');
 f.enable();assert.equal((await capability('start',f.environment,input('turn-two','explicit retry','continue'))).state,'generating');assert.equal(f.counts().sends,1);
 const changed=page({disabled:true});await capability('start',changed.environment,input('turn-one','prepared'));
 changed.environment.document.querySelector('textarea').value='personal replacement';changed.enable();
 await assert.rejects(capability('start',changed.environment,input('turn-two','retry','continue')),/existing draft/);assert.equal(changed.counts().sends,0);
});
test('stop is bound to the current turn and preserves stopped rather than calling partial text complete',async()=>{
 const f=page();await capability('start',f.environment,input('turn-one'));await assert.rejects(capability('stop',f.environment,{turn_id:'wrong'}),/matching/);assert.equal(f.counts().stops,0);
 assert.equal((await capability('stop',f.environment,{turn_id:'turn-one'})).state,'stopped');assert.equal((await capability('read',f.environment,{turn_id:'turn-one'})).state,'stopped');assert.equal(f.counts().stops,1);
 assert.equal((await capability('stop',f.environment,{turn_id:'turn-one'})).state,'stopped');assert.equal(f.counts().stops,1);
});
test('observed sibling service banner is an error, quoted prompt words are not',async()=>{
 const f=page();await capability('start',f.environment,input('turn-one'));f.error({quoted:true});assert.equal((await capability('read',f.environment,{turn_id:'turn-one'})).state,'pending');
 f.error();const result=await capability('read',f.environment,{turn_id:'turn-one'});assert.equal(result.state,'error');assert.equal(result.error,'DeepSeek 服务器暂时不可用。');
});
test('old error banners and continuation controls do not contaminate the next turn',async()=>{
 const f=page();await capability('start',f.environment,input('turn-one'));f.error();f.continuation();assert.equal((await capability('read',f.environment,{turn_id:'turn-one'})).state,'error');
 await capability('start',f.environment,input('turn-two','new recovery instruction','continue'));f.finish('new answer');assert.equal((await capability('read',f.environment,{turn_id:'turn-two'})).state,'complete');
 f.continuation();assert.equal((await capability('read',f.environment,{turn_id:'turn-two'})).state,'stopped');
});
