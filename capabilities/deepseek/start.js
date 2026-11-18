async function (input) {
  if (location.origin !== 'https://chat.deepseek.com') throw new Error('DeepSeek origin required');
  if (!['new','continue'].includes(input.mode) || !/^[A-Za-z0-9_-]{1,100}$/.test(input.turn_id || '')) throw new Error('Explicit mode and turn_id are required');
  if (typeof input.prompt !== 'string' || !input.prompt.trim() || input.prompt.length > 160000) throw new Error('A prompt of 1–160000 characters is required');
  const previous = globalThis.__LRMCP_DEEPSEEK_SESSION__;
  if (previous?.turn_id === input.turn_id) throw new Error('This turn was already started; read it instead of sending again');
  const normalize = text => (text || '').replace(/\s+/gu, ' ').trim();
  const editor = document.querySelector('textarea[placeholder*="DeepSeek"]');
  const early = state => ({state,text:'',turn_id:input.turn_id,sent:false});
  if (!editor) return early(document.querySelector('input[type="password"],a[href*="sign_in"],a[href*="login"]') ? 'login_required' : 'not_ready');
  const baseline = [...document.querySelectorAll('.ds-message')];
  if (input.mode === 'new' && (previous?.owned || baseline.length || /^\/a\/chat\/s\//.test(location.pathname))) throw new Error('A new session requires an empty conversation');
  if (input.mode === 'continue' && (!previous?.owned || typeof previous.observe !== 'function')) throw new Error('Continue requires this runtime-owned conversation');
  const composerFor = () => {
    let root = document.querySelector('textarea[placeholder*="DeepSeek"]')?.parentElement;
    for (let i=0;i<4 && root && !root.querySelector('.ds-button--primary.ds-button--circle');i++) root=root.parentElement;
    return root;
  };
  const composer = composerFor();
  if (!composer?.querySelector('.ds-button--primary.ds-button--circle')) return early('unsupported');
  if (composer.querySelector('.ds-loading,svg[data-icon="spin"],svg path[d^="M2 4.88"]')) throw new Error('The previous turn is still generating');
  if (editor.value.trim() && !(input.mode === 'continue' && previous?.preparedPrompt === editor.value)) throw new Error('DeepSeek composer contains an existing draft');
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value')?.set;
  if (!setter) return early('unsupported');
  const originalDraft = editor.value;
  const setDraft = (target, value) => {
    setter.call(target,value);
    target.dispatchEvent(new Event('input',{bubbles:true}));
  };
  const turn={owned:true,turn_id:input.turn_id,baseline:new Set(baseline),expected:normalize(input.prompt),preparedPrompt:input.prompt,user:null,assistant:null,stopped:false,observe:null};
  turn.observe=()=>{
    if (globalThis.__LRMCP_DEEPSEEK_SESSION__!==turn) throw new Error('DeepSeek turn changed');
    const result=(state,text='',error)=>({state,text,turn_id:turn.turn_id,...(error?{error}: {})});
    if (turn.preparedPrompt !== null) return result('not_ready');
    const messages=[...document.querySelectorAll('.ds-message')];
    if (!turn.user || !messages.includes(turn.user)) turn.user=messages.find(node=>!turn.baseline.has(node) && normalize(node.querySelector('.ds-collapsible-text')?.textContent)===turn.expected) || null;
    if (!turn.user) return result('pending');
    const afterUser=node=>!!(turn.user.compareDocumentPosition(node)&Node.DOCUMENT_POSITION_FOLLOWING);
    const unavailable=[...document.querySelectorAll('div > span:first-child')].find(span=>span.children.length===0 && span.textContent?.trim()==='服务器暂时不可用' && !span.closest('.ds-message') && span.nextElementSibling?.querySelector('[role="button"],button') && afterUser(span));
    if (unavailable) return result('error','','DeepSeek 服务器暂时不可用。');
    turn.assistant=messages.slice(messages.indexOf(turn.user)+1).find(node=>!node.querySelector('.ds-collapsible-text')) || null;
    const answer=turn.assistant?.querySelector('.ds-assistant-message-main-content');
    const text=answer?.innerText || '';
    const error=turn.assistant?.querySelector('[role="alert"]');
    if (error) return result('error','',error.innerText.slice(0,1000));
    const currentComposer=composerFor();
    if (!currentComposer?.querySelector('.ds-button--primary.ds-button--circle')) return result('generating',text);
    if (currentComposer.querySelector('.ds-loading,svg[data-icon="spin"],svg path[d^="M2 4.88"]')) return result('generating',text);
    const continuation=[...document.querySelectorAll('[role="button"],button')].some(button=>button.textContent?.trim()==='继续生成' && !button.closest('a,nav,aside,.ds-assistant-message-main-content,.ds-think-content') && afterUser(button));
    if (turn.stopped || continuation) return result('stopped',text);
    return result(answer && text?'complete':'pending',text);
  };
  globalThis.__LRMCP_DEEPSEEK_SESSION__=turn;
  setDraft(editor,input.prompt);
  const enabled = button => button && !button.disabled && button.getAttribute('aria-disabled')!=='true' && !button.classList.contains('ds-button--disabled');
  const deadlineToSend = Date.now()+2400;
  await new Promise(resolve=>setTimeout(resolve,0));
  let send, prepared;
  while (globalThis.__LRMCP_DEEPSEEK_SESSION__===turn) {
    prepared=document.querySelector('textarea[placeholder*="DeepSeek"]');
    send=composerFor()?.querySelector('.ds-button--primary.ds-button--circle');
    if (prepared?.value!==input.prompt || enabled(send) || Date.now()>=deadlineToSend) break;
    await new Promise(resolve=>setTimeout(resolve,50));
  }
  if (globalThis.__LRMCP_DEEPSEEK_SESSION__!==turn) return early('not_ready');
  if (prepared?.value!==input.prompt || !enabled(send)) {
    if (prepared?.value===input.prompt) setDraft(prepared,originalDraft);
    globalThis.__LRMCP_DEEPSEEK_SESSION__=previous;
    return early('not_ready');
  }
  turn.preparedPrompt=null;
  send.click();
  const deadline=Date.now()+5000;
  while(Date.now()<deadline){const result=turn.observe();if(turn.user)return {...result,sent:true};await new Promise(resolve=>setTimeout(resolve,50));}
  return {state:'unconfirmed',text:'',turn_id:turn.turn_id,sent:true};
}
