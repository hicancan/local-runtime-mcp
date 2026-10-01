import assert from 'node:assert/strict';
import { test } from 'node:test';
import { commandStream } from '../../internal/browser/extension/connection.js';

const peer = { instance_id: 'profile', boot_id: 'worker-boot' };
function fixture(t) {
  const saved = { socket: globalThis.WebSocket, set: globalThis.setTimeout, clear: globalThis.clearTimeout };
  const timers = new Map(); let next = 0, now = 0;
  globalThis.setTimeout = (callback, delay) => { const id=++next; timers.set(id,{callback,at:now+delay}); return id; };
  globalThis.clearTimeout = id => timers.delete(id);
  class Socket {
    static instances=[];
    sent=[]; closed=false;
    constructor(url) { this.url=url; Socket.instances.push(this); }
    send(value) { assert.equal(this.closed,false); this.sent.push(JSON.parse(value)); }
    close() { this.closed=true; this.onclose?.(); }
    message(value) { this.onmessage?.({data:JSON.stringify(value)}); }
  }
  globalThis.WebSocket=Socket;
  t.after(()=>{ globalThis.WebSocket=saved.socket; globalThis.setTimeout=saved.set; globalThis.clearTimeout=saved.clear; });
  return { Socket, timers, tick(ms) { now+=ms; for(const [id,timer] of [...timers]) if(timer.at<=now){timers.delete(id);timer.callback();} } };
}

test('idle command stream stays authenticated through genuine twenty-second WebSocket traffic', async t => {
  const f=fixture(t), abort=new AbortController(), commands=[], ready=[];
  const pending=commandStream('127.0.0.1:9315','secret token',peer,abort.signal,id=>ready.push(id),value=>commands.push(value));
  const socket=f.Socket.instances[0];
  assert.equal(socket.url,'ws://127.0.0.1:9315/v1/connect');
  assert.equal(socket.url.includes('secret'),false);
  socket.onopen(); assert.deepEqual(socket.sent,[{type:'authenticate',token:'secret token',peer}]);
  socket.message({type:'ready',bridge_id:'bridge'});
  for(let i=0;i<6;i++){ f.tick(20000); socket.message({type:'keepalive'}); assert.equal(socket.closed,false); }
  assert.deepEqual(ready,['bridge']); assert.equal(socket.sent.filter(m=>m.type==='keepalive').length,6);
  socket.message({id:'one',boot_id:peer.boot_id,method:'tabs.list'}); assert.equal(commands.length,1);
  abort.abort(); await pending; assert.equal(socket.closed,true); assert.equal(f.timers.size,0);
});

test('silence closes the stream, releases handlers, and never executes a stale connection command', async t => {
  const f=fixture(t), abort=new AbortController(); let calls=0;
  const pending=commandStream('localhost:9315','credential',peer,abort.signal,()=>{},()=>calls++);
  const rejected=assert.rejects(pending,/keepalive timed out/);
  const socket=f.Socket.instances[0]; socket.onopen(); socket.message({type:'ready',bridge_id:'bridge'});
  f.tick(35000); await rejected;
  socket.message({id:'late',boot_id:peer.boot_id,method:'tabs.list'});
  assert.equal(calls,0); assert.equal(socket.closed,true); assert.equal(socket.onmessage,null); assert.equal(f.timers.size,0);
});

test('authentication deadline and generation validation fail closed without command replay', async t => {
  const f=fixture(t), abort=new AbortController(); let calls=0;
  const timeout=commandStream('localhost:9315','credential',peer,abort.signal,()=>{},()=>calls++);
  const timedOut=assert.rejects(timeout,/authentication timed out/); f.tick(5000); await timedOut;
  const pending=commandStream('localhost:9315','credential',peer,abort.signal,()=>{},()=>calls++);
  const rejected=assert.rejects(pending,/generation is stale/);
  const socket=f.Socket.instances[1];socket.onopen();socket.message({type:'ready',bridge_id:'bridge'});
  socket.message({id:'old',boot_id:'previous-worker',method:'tabs.list'}); await rejected; assert.equal(calls,0);
});

test('offline worker retries once, rests until an alarm, and never starts duplicate reconnect loops', async t => {
  const f=fixture(t);
  const originalChrome=globalThis.chrome, originalInterval=globalThis.setInterval, originalWarn=console.warn;
  let alarm, reads=0;
  const event={addListener(){}};
  globalThis.chrome={
    runtime:{onMessage:event}, debugger:{onDetach:event,onEvent:event},
    tabs:{onRemoved:event,onUpdated:event},
    storage:{local:{async get(){reads++;return {address:'127.0.0.1:9315',token:'0123456789abcdef0123456789abcdef',instanceId:'profile',label:'test'};}}},
    alarms:{onAlarm:{addListener(listener){alarm=listener;}},async create(){}}
  };
  globalThis.setInterval=()=>0; console.warn=()=>{};
  t.after(()=>{globalThis.chrome=originalChrome;globalThis.setInterval=originalInterval;console.warn=originalWarn;});
  const Base=f.Socket;
  globalThis.WebSocket=class extends Base {constructor(url){super(url);queueMicrotask(()=>this.onerror?.());}};
  await import('../../internal/browser/extension/service_worker.js?offline-lifecycle');
  const settle=async()=>{for(let i=0;i<12;i++)await Promise.resolve();};
  await settle(); assert.equal(reads,1);
  alarm({name:'bridge-reconnect'});alarm({name:'bridge-reconnect'}); await settle();assert.equal(reads,1);
  f.tick(1000);await settle();assert.equal(reads,2);assert.equal(f.timers.size,0);
  f.tick(60000);await settle();assert.equal(reads,2);
  alarm({name:'bridge-reconnect'});alarm({name:'bridge-reconnect'});await settle();assert.equal(reads,3);
  f.tick(1000);await settle();assert.equal(reads,4);assert.equal(f.timers.size,0);
});
