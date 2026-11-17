async function (input) {
  if(location.origin!=='https://chat.deepseek.com')throw new Error('DeepSeek origin required');
  const turn=globalThis.__LRMCP_DEEPSEEK_SESSION__;
  if(!input.turn_id || !turn?.owned || turn.turn_id!==input.turn_id)throw new Error('Stop requires the matching runtime-owned turn');
  const observed=turn.observe();
  if(['complete','stopped'].includes(observed.state))return observed;
  const editor=document.querySelector('textarea[placeholder*="DeepSeek"]');
  let composer=editor?.parentElement;
  for(let i=0;i<4&&composer&&!composer.querySelector('.ds-button--primary.ds-button--circle');i++)composer=composer.parentElement;
  const stopIcon=composer?.querySelector('svg path[d^="M2 4.88"]');
  const stop=stopIcon?.closest('[role="button"],button');
  if(!stop || stop.getAttribute('aria-disabled')==='true' || stop.classList.contains('ds-button--disabled'))return {state:'unconfirmed',text:observed.text,turn_id:turn.turn_id};
  stop.click();
  const deadline=Date.now()+2500;
  while(Date.now()<deadline){
    if(globalThis.__LRMCP_DEEPSEEK_SESSION__!==turn)throw new Error('DeepSeek turn changed during stop');
    if(!composer.querySelector('.ds-loading,svg[data-icon="spin"],svg path[d^="M2 4.88"]')){turn.stopped=true;return {state:'stopped',text:turn.observe().text,turn_id:turn.turn_id};}
    await new Promise(resolve=>setTimeout(resolve,50));
  }
  return {state:'unconfirmed',text:turn.observe().text,turn_id:turn.turn_id};
}
