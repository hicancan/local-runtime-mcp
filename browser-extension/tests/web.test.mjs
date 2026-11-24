import test from 'node:test';
import assert from 'node:assert/strict';
import { webMessage, webSource, readWebStream, WEB_PROTOCOL } from '../../internal/browser/extension/web_protocol.js';
import { installWebBridge } from '../../internal/browser/extension/web_bridge.js';
import { createLRMCPClient, LocalRuntimeError } from '../../internal/browser/extension/web_sdk.js';

const sender={id:'extension',origin:'https://allowed.example',url:'https://allowed.example/page',tab:{id:7},frameId:0,documentId:'doc-one'};
const request={protocol:WEB_PROTOCOL,kind:'request',id:'one',method:'capabilities.invoke',params:{name:'icourse.outline',input:{}}};
const stream=lines=>new Response(lines.map(line=>JSON.stringify(line)).join('\n')+'\n',{headers:{'content-type':'application/x-ndjson'}});
const settle=async()=>{for(let i=0;i<25;i++)await Promise.resolve();};

test('web source is browser-supplied and rejects foreign, opaque, model and iframe senders',()=>{
  assert.deepEqual(webSource(sender,'extension'),{origin:sender.origin,url:sender.url,frame_id:0,document_id:'doc-one',nativeTabID:7});
  for(const changed of [{id:'other'},{origin:'https://forged.example'},{origin:'null'},{frameId:1},{documentId:undefined},{url:'file:///private'}])assert.throws(()=>webSource({...sender,...changed},'extension'));
  assert.equal(webMessage({...request,method:'evaluate'}),null);
  assert.equal(webMessage({...request,method:'models.chat'}),null);
  assert.equal(webMessage({...request,params:[]}),null);
  assert.equal(webMessage({...request,params:{large:'x'.repeat(1024*1024)}}),null);
  assert.equal('origin' in webMessage({...request,origin:'https://forged.example'}),false);
});

test('NDJSON forwards incremental events, validates termination and does not accept arbitrary records',async()=>{
  const seen=[];
  await readWebStream(stream([{type:'event',data:{type:'progress',data:'first'}},{type:'result',result:{content:'first'}}]),value=>seen.push(value));
  assert.equal(seen[0].data.data,'first');assert.equal(seen[1].type,'result');
  await assert.rejects(readWebStream(stream([{type:'event',data:{type:'keepalive'}}]),()=>{}),/before confirming/);
  await assert.rejects(readWebStream(stream([{type:'eval',script:'bad'}]),()=>{}),/invalid web bridge/);
  await assert.rejects(readWebStream(stream([{type:'result',result:1},{type:'event',data:2}]),()=>{}),/after its final/);
});

test('worker relays only capability methods, binds real sender and never returns its credential',async t=>{
  const saved={chrome:globalThis.chrome,fetch:globalThis.fetch};t.after(()=>Object.assign(globalThis,saved));
  let connected;const posted=[],calls=[];
  globalThis.chrome={runtime:{id:'extension',onConnect:{addListener(fn){connected=fn;}}}};
  globalThis.fetch=async(url,options)=>{calls.push({url,options});return stream([{type:'event',data:{type:'progress',data:'ok'}},{type:'result',result:{text:'ok'}}]);};
  installWebBridge(async()=>({address:'127.0.0.1:9315',token:'test-credential-32-characters-long',instanceId:'browser-one',bootId:'boot-one'}),id=>'boot-one.tab-'+id);
  let message,disconnected;
  connected({name:WEB_PROTOCOL,sender,postMessage(value){posted.push(value);},disconnect(){},onMessage:{addListener(fn){message=fn;}},onDisconnect:{addListener(fn){disconnected=fn;}}});
  message({...request,origin:'https://forged.example',tab_id:'victim'});await settle();
  assert.equal(calls.length,1);const body=JSON.parse(calls[0].options.body);
  assert.equal(body.source.origin,sender.origin);assert.equal(body.source.tab_id,'boot-one.tab-7');assert.equal(body.source.document_id,'doc-one');
  assert.equal(body.instance_id,'browser-one');assert.equal(body.request.method,'capabilities.invoke');
  assert.equal(JSON.stringify(posted).includes('credential'),false);
  assert.equal(posted.at(-1).kind,'result');
  message(request);await settle();assert.equal(calls.length,1);assert.match(posted.at(-1).error,/duplicate/);
  disconnected();
});

class Page extends EventTarget {
  location={origin:'https://allowed.example'};sent=[];
  postMessage(value,origin){assert.equal(origin,this.location.origin);this.sent.push(value);}
  reply(value,origin=this.location.origin){const event=new Event('message');Object.defineProperties(event,{data:{value},origin:{value:origin},source:{value:this}});this.dispatchEvent(event);}
}
test('SDK streams capability events and retains the final result without exposing an extension ID',async()=>{
  const page=new Page(),client=createLRMCPClient(page),call=client.request('capabilities.invoke',{name:'icourse.outline',input:{}});const id=page.sent[0].id;
  page.reply({protocol:WEB_PROTOCOL,id,kind:'event',data:{type:'progress',data:'hi'}},'https://other.example');
  page.reply({protocol:WEB_PROTOCOL,id,kind:'event',data:{type:'progress',data:'hello'}});
  page.reply({protocol:WEB_PROTOCOL,id,kind:'event',data:{type:'chunk',data:{name:'example'}}});
  page.reply({protocol:WEB_PROTOCOL,id,kind:'result',data:{ok:true,content:'hello'}});
  const seen=[];for await(const item of call)seen.push(item);assert.deepEqual(seen.map(value=>value.type),['progress','chunk']);
  assert.deepEqual(await call.result,{ok:true,content:'hello'});assert.equal(page.sent.length,1);
});
test('SDK abort cancels only its own request and reports an unknown remote outcome',async()=>{
  const page=new Page(),abort=new AbortController(),call=createLRMCPClient(page).request('capabilities.invoke',{name:'icourse.outline',input:{}},{signal:abort.signal});
  abort.abort();await assert.rejects(call.result,/outcome may be unknown/);assert.equal(page.sent[1].kind,'cancel');assert.equal(page.sent[1].id,page.sent[0].id);
  assert.equal(page.sent.filter(value=>value.kind==='request').length,1);
});

test('SDK preserves optional public source error codes without interpreting provider prose',async()=>{
  const page=new Page(),call=createLRMCPClient(page).request('capabilities.invoke',{name:'icourse.outline',input:{}}),id=page.sent[0].id;
  page.reply({protocol:WEB_PROTOCOL,id,kind:'error',error:'public provider text',code:'provider_unavailable'});
  await assert.rejects(call.result,error=>error instanceof LocalRuntimeError&&error.code==='provider_unavailable'&&error.message==='public provider text');
});

test('SDK lists and invokes capabilities without accepting tab or origin overrides',async()=>{
  const page=new Page(),client=createLRMCPClient(page),pending=client.invoke('icourse.outline',{});const message=page.sent[0];
  assert.equal(message.method,'capabilities.invoke');assert.deepEqual(message.params,{name:'icourse.outline',input:{}});
  assert(webMessage(message));assert.equal('tab_id' in message.params,false);
  page.reply({protocol:WEB_PROTOCOL,id:message.id,kind:'result',data:{course:{title:'Example'}}});assert.deepEqual(await pending,{course:{title:'Example'}});
});
