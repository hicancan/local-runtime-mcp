function (input) {
  if (location.origin !== 'https://chat.deepseek.com') throw new Error('DeepSeek origin required');
  const session=globalThis.__LRMCP_DEEPSEEK_SESSION__;
  if(input.turn_id && (!session?.owned || session.turn_id!==input.turn_id)) throw new Error('DeepSeek turn identity changed; no earlier answer was reused');
  if(session?.owned && typeof session.observe==='function')return session.observe();
  const result=state=>({state,text:'',turn_id:''});
  const editor=document.querySelector('textarea[placeholder*="DeepSeek"]');
  if(!editor)return result(document.querySelector('input[type="password"],a[href*="sign_in"],a[href*="login"]')?'login_required':'not_ready');
  let composer=editor.parentElement;
  for(let i=0;i<4&&composer&&!composer.querySelector('.ds-button--primary.ds-button--circle');i++)composer=composer.parentElement;
  if(!composer?.querySelector('.ds-button--primary.ds-button--circle'))return result('not_ready');
  // Readiness probe never imports an existing user conversation into a runtime session.
  return result(document.querySelector('.ds-message') || /^\/a\/chat\/s\//.test(location.pathname)?'unsupported':'idle');
}
