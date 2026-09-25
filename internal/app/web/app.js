'use strict';
let csrf = '', user = null, jobs = [], targets = [], runOffset = 0;
const $ = s => document.querySelector(s);
function node(tag, text, cls) { const n = document.createElement(tag); if (text !== undefined) n.textContent = text; if (cls) n.className = cls; return n; }
function notice(text) { $('#notice').textContent = text; $('#notice').hidden = !text; }
function time(value) { return value ? new Date(value).toLocaleString() : '—'; }
async function api(path, method = 'GET', data, headers = {}) {
  const opts = {method, headers: {'X-CSRF-Token': csrf, ...headers}};
  if (data !== undefined) { opts.headers['Content-Type'] = 'application/json'; opts.body = JSON.stringify(data); }
  const response = await fetch('/api' + path, opts);
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || `HTTP ${response.status}`);
  return result;
}
function action(label, fn) { const b = node('button', label); b.type = 'button'; b.addEventListener('click', () => perform(fn, b)); return b; }
async function perform(fn, button) { notice(''); if (button) button.disabled = true; try { await fn(); } catch (e) { notice(e.message); } finally { if (button) button.disabled = false; } }
function table(container, headings, rows) {
  const root = $(container); root.replaceChildren();
  if (!rows.length) { root.append(node('div', 'Nothing here yet.', 'empty')); return; }
  const wrap = node('div', undefined, 'table-wrap'), t = node('table'), head = node('tr');
  for (const title of headings) head.append(node('th', title));
  const thead = node('thead'); thead.append(head); t.append(thead); const tbody = node('tbody');
  for (const row of rows) { const tr = node('tr'); for (const item of row) { const td = node('td'); if (item instanceof Node) td.append(item); else td.textContent = item; tr.append(td); } tbody.append(tr); }
  t.append(tbody); wrap.append(t); root.append(wrap);
}
function buttons(...items) { const n = node('div'); for (const item of items) n.append(item); return n; }
function badge(status) { return node('span', status, 'badge ' + status); }
function form(selector, fn) { const f = $(selector); f.addEventListener('submit', e => { e.preventDefault(); perform(() => fn(f), f.querySelector('button:not([type="button"])')); }); }
function value(f, key) { return f.elements.namedItem(key).value; }
function jsonField(f,key){ try { return JSON.parse(value(f,key)); } catch { throw new Error(`${key} must be valid JSON`); } }
function plain(f, keys) { return Object.fromEntries(keys.map(k => [k, value(f,k)])); }
function checked(f,k) { return f.elements.namedItem(k).checked; }
function selectOptions(selector, items) { document.querySelectorAll(selector).forEach(select => { const previous = select.value; select.replaceChildren(); for (const item of items) { const option = node('option', item.label); option.value = item.id; select.append(option); } if (items.some(x => x.id === previous)) select.value = previous; }); }
async function loadTargets() { targets = await api('/targets'); selectOptions('.target-select', targets.map(t => ({id:t.id,label:`${t.id} · ${t.sprite_name}`}))); table('#target-list',['ID','Sprite','Organization','HTTP URL'],targets.map(t => [t.id,t.sprite_name,t.organization,t.url])); }
async function loadCredentials(){ const items = await api('/credentials'); selectOptions('.credential-select', items.filter(c => c.kind === 'sprite')); table('#credential-list',['ID','Label','Kind','Version'],items.map(c=>[c.id,c.label,c.kind,c.version])); }
async function loadJobs() {
  jobs = await api('/jobs'); table('#job-list',['Job','Schedule','Target','Next run','State','Actions'], jobs.map(j => {
    const actions = buttons(action('History', async()=>{showTab('runs');await loadRuns(j.id);}));
    if(user.role !== 'reader') actions.append(action('Edit', ()=>editJob(j)),action(j.enabled?'Pause':'Enable',async()=>{await api('/jobs/'+j.id,'PUT',{...j,enabled:!j.enabled});await loadJobs();}),action('Run now',async()=>{const r=await api('/jobs/'+j.id+'/runs','POST',undefined,{'Idempotency-Key':crypto.randomUUID()});notice('Queued run '+r.id);}),action('Delete',async()=>{if(!confirm('Delete this job and cancel its queued runs?')) return;await api('/jobs/'+j.id,'DELETE',undefined,{'If-Match':String(j.revision)});await loadJobs();}));
    return [j.name,j.schedule+' · '+j.timezone,j.target_id,j.enabled?time(j.next_run_at):'Paused',badge(j.enabled?'enabled':'paused'),actions];
  }));
}
function executionFields(){const http=$('#job-type').value==='http';$('#exec-fields').hidden=http;$('#http-fields').hidden=!http;$('#argv-field').hidden=$('#job-type').value==='shell';$('#script-field').hidden=$('#job-type').value!=='shell';}
function editJob(j){ const f=$('#job-form');f.reset();const values={...j,...j.policy,...j.execution};for(const key of ['id','name','target_id','schedule','timezone','revision','type','script','shell','directory','method','path','body','timeout_seconds','start_grace_seconds','overlap','misfire','max_attempts']){if(values[key]!==undefined)f.elements.namedItem(key).value=values[key];}for(const key of ['argv','env','headers','success_statuses'])f.elements.namedItem(key).value=JSON.stringify(j.execution[key]??(key==='argv'||key==='success_statuses'?[]:{}));f.elements.enabled.checked=j.enabled;f.elements.retry_safe.checked=j.policy.retry_safe;$('#job-editor').open=true;executionFields();$('#job-editor').scrollIntoView({behavior:'smooth'}); }
let runRows=[], runJobID;
async function loadRuns(jobID,more=false){if(!more){runOffset=0;runRows=[];runJobID=jobID;}jobID=runJobID;const result=await api('/runs?offset='+runOffset+(jobID?'&job_id='+encodeURIComponent(jobID):''));runOffset=result.next_offset;runRows.push(...result.runs);$('#more-runs').hidden=!result.has_more;table('#run-list',['Run / job','Scheduled','Outcome','Attempt','Actions'],runRows.map(r=>{const actions=buttons(action('Details',async()=>{const data=await api('/runs/'+r.id);$('#run-detail').hidden=false;$('#run-detail pre').textContent=JSON.stringify(data,null,2);}));if(user.role!=='reader'){if(['queued','dispatching','running','retry_wait','unknown'].includes(r.status))actions.append(action('Cancel',async()=>{await api('/runs/'+r.id+'/cancel','POST',{});await loadRuns();}));if(r.status==='unknown'&&!r.resolved)actions.append(action('Resolve',async()=>{const note=prompt('Confirm you investigated this run. Explain why releasing its overlap block is safe (remote work may still be running):');if(!note)return;await api('/runs/'+r.id+'/resolve','POST',{accept_risk:true,note});await loadRuns();}));if(['failed','timed_out','cancelled'].includes(r.status)||r.status==='unknown'&&r.resolved)actions.append(action('Retry',async()=>{if(!confirm('Retry the same run ID? The saved job must be declared safe to repeat.'))return;await api('/runs/'+r.id+'/retry','POST',{});await loadRuns();}));}return[r.id.slice(0,8)+' / '+(jobs.find(j=>j.id===r.job_id)?.name||r.job_id),time(r.scheduled_at),badge(r.status+(r.resolved?' (resolved)':'')),r.attempt,actions];}));}
async function loadTokens(){const tokens=await api('/tokens');table('#token-list',['Name','Scopes','Targets','Expires','Last used','Actions'],tokens.map(t=>[t.name,t.scopes.join(', '),t.targets.join(', '),time(t.expires_at),time(t.last_used_at),t.revoked?'Revoked':action('Revoke',async()=>{await api('/tokens/'+t.id,'DELETE');await loadTokens();})]));}
async function loadAudit(){const [events,status]=await Promise.all([api('/audit'),api('/status')]);$('#system-status').textContent=JSON.stringify(status,null,2);table('#audit-list',['When','Who','Action','Object','Detail'],events.map(e=>[time(e.at),e.actor,e.action,e.object,e.detail]));}
function showTab(id){document.querySelectorAll('.tab').forEach(x=>x.hidden=x.id!==id);document.querySelectorAll('nav button').forEach(b=>b.classList.toggle('active',b.dataset.tab===id));$('#new-token').textContent='';$('#new-token').hidden=true;}
async function boot(){try {const me=await api('/me');user=me.user;csrf=me.csrf;}catch{$('#login').hidden=false;return;}$('#login').hidden=true;$('#workspace').hidden=false;$('#logout').hidden=false;$('#identity').textContent=user.username+' · '+user.role;document.querySelectorAll('.admin').forEach(e=>e.hidden=user.role!=='admin');document.querySelectorAll('.write').forEach(e=>e.hidden=user.role==='reader');await loadTargets();if(user.role==='admin')await loadCredentials();await loadJobs();}
form('#login-form',async f=>{const result=await api('/login','POST',plain(f,['username','password']));csrf=result.csrf;f.reset();await boot();});
$('#logout').addEventListener('click',()=>perform(async()=>{await api('/logout','POST',{});location.reload();}));
document.querySelectorAll('nav button').forEach(b=>b.addEventListener('click',()=>perform(async()=>{showTab(b.dataset.tab);switch(b.dataset.tab){case'jobs':await loadJobs();break;case'runs':await loadRuns();break;case'targets':await loadTargets();break;case'credentials':await loadCredentials();break;case'account':await loadTokens();break;case'audit':await loadAudit();break;}})));
$('#new-job').addEventListener('click',()=>{const f=$('#job-form');f.reset();f.elements.id.value='';f.elements.revision.value='';$('#job-editor').open=true;executionFields();});
$('#job-type').addEventListener('change',executionFields);
$('#preview').addEventListener('click',()=>perform(async()=>{$('#schedule-preview').textContent=(await api('/schedules/preview','POST',plain($('#job-form'),['schedule','timezone']))).join('\n');}));
form('#job-form',async f=>{const j=plain(f,['name','target_id','schedule','timezone']);j.enabled=checked(f,'enabled');j.policy={...plain(f,['overlap','misfire']),retry_safe:checked(f,'retry_safe')};for(const k of ['timeout_seconds','start_grace_seconds','max_attempts'])j.policy[k]=Number(value(f,k));const type=value(f,'type');j.execution=type!=='http'?{type,...(type==='shell'?{script:value(f,'script'),shell:value(f,'shell')}:{argv:jsonField(f,'argv')}),directory:value(f,'directory'),env:jsonField(f,'env')}:{type,...plain(f,['method','path','body']),headers:jsonField(f,'headers'),success_statuses:jsonField(f,'success_statuses')};const id=value(f,'id');if(id)j.revision=Number(value(f,'revision'));await api('/jobs'+(id?'/'+id:''),id?'PUT':'POST',j);$('#job-editor').open=false;await loadJobs();notice('Job saved.');});
form('#credential-form',async f=>{const data=plain(f,['id','label','kind','value']);const rotate=checked(f,'rotate');await api('/credentials'+(rotate?'/'+data.id:''),rotate?'PUT':'POST',data);f.reset();await loadCredentials();notice('Credential saved.');});
form('#target-form',async f=>{await api('/targets','POST',{...plain(f,['id','sprite_name','credential_id','http_credential_id','app_credential_id','app_header']),public:checked(f,'public')});f.reset();await loadTargets();notice('Target verified and registered.');});
form('#token-form',async f=>{const data={...plain(f,['name','password']),scopes:value(f,'scopes').split(',').map(x=>x.trim()),targets:value(f,'targets').split(',').map(x=>x.trim()),expires_hours:Number(value(f,'expires_hours'))};const result=await api('/tokens','POST',data);f.elements.password.value='';$('#new-token').hidden=false;$('#new-token').textContent='Copy this token now. It will not be shown again.\n\n'+result.token;await loadTokens();});
form('#password-form',async f=>{await api('/password','POST',plain(f,['current','new']));location.reload();});
$('#refresh-runs').addEventListener('click',()=>perform(()=>loadRuns()));$('#more-runs').addEventListener('click',()=>perform(()=>loadRuns(undefined,true)));
perform(boot);
