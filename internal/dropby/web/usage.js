'use strict';

const usageWeekdays=['周日','周一','周二','周三','周四','周五','周六'];
const usageHours=['00–06 时','06–12 时','12–18 时','18–24 时'];
const usageGroups={member:'成员',purpose:'用途',matter:'事项'};
const usagePurposes={chat:'日常交流',research:'研究与探索',compact:'上下文压缩',search:'网页搜索',read_page:'网页读取',control:'指令理解',preferences:'偏好设置',model:'模型调用'};
const usageOutcomes={incomplete:'模型未完成输出',tool_calls:'生成了工具调用',no_message:'未生成消息正文',generated_message:'生成了消息正文',released:'预留已释放',empty:'没有可用结果，本次免费'};
const usagePageSize=2;
let usageView;

function usageMoney(micros, digits=micros>0 && micros<10000 ? 5 : 2) {
  return '$'+(micros/1e6).toFixed(digits);
}
function usageDate(date) { return new Date(date+'T12:00:00Z'); }
function usageShift(date, days) {
  // 服务端已按预算时区给出日期；用 UTC 做纯日历运算，避免客户端时区及夏令时改变日期。
  const d=usageDate(date);
  d.setUTCDate(d.getUTCDate()+days);
  return d.toISOString().slice(0,10);
}
function usageMonday(date) { return usageShift(date,-((usageDate(date).getUTCDay()+6)%7)); }
function usageDateLabel(date) { return Number(date.slice(5,7))+'/'+Number(date.slice(8,10)); }
function usageSum(rows, key='cost_micros') { return rows.reduce((total,row)=>total+row[key],0); }

function openUsageAnalysis() {
  showWorkspace('额度分析','', '<div id="usage-analysis" class="usage-analysis"></div>','usage');
  usageView={root:$('usage-analysis'),period:'week',date:'',group:'member',filters:{},filterNames:{},showFilters:false,limit:usagePageSize,sequence:0};
  loadUsageAnalysis('week','');
}

async function loadUsageAnalysis(period,date,quiet=false) {
  const view=usageView;
  if (!view?.root.isConnected) return;
  const sequence=++view.sequence;
  view.loading=true;
  if (!quiet) {
    view.request={period,date};
    view.root.setAttribute('aria-busy','true');
    if (!view.report) view.root.innerHTML='<p class="usage-notice" role="status">正在读取额度记录…</p>';
  }
  const budget=JSON.stringify(state.usage);
  try {
    const query=new URLSearchParams({period});
    if (date) query.set('date',date);
    const report=await api('/api/usage?'+query);
    if (!view.root.isConnected || sequence!==view.sequence) return;
    if (quiet && view.root.contains(document.activeElement) && document.activeElement.matches('select')) return;
    const same=quiet && JSON.stringify(view.report)===JSON.stringify(report);
    view.report=report;
    view.period=period;
    view.date=date || report.today;
    view.budget=budget;
    view.error=false;
    if (!quiet) view.limit=usagePageSize;
    if (!same) renderUsageAnalysis(view);
  } catch {
    if (!view.root.isConnected || sequence!==view.sequence || quiet) return;
    view.error=true;
    renderUsageAnalysis(view);
    view.root.querySelector('[data-action="usage-retry"]')?.focus({preventScroll:true});
  } finally {
    if (sequence===view.sequence) {
      view.loading=false;
      view.root.removeAttribute('aria-busy');
    }
  }
}

function refreshUsageAnalysis() {
  const view=usageView;
  // SSE 刷新可更新在途用量；浏览历史或正在操作下拉菜单时保留当前画面。
  if (!view?.root.isConnected || !view.report || view.loading || view.error) return;
  const running=view.report.entries.some(row=>row.status==='running');
  if (!running && (view.report.end<=view.report.today || view.budget===JSON.stringify(state.usage))) return;
  if (view.root.contains(document.activeElement) && document.activeElement.matches('select')) return;
  loadUsageAnalysis(view.period,view.date,true);
}

function usageChoice(label,action,value,active) {
  return `<button type="button" data-action="${action}" data-value="${escapeHTML(value)}" aria-pressed="${active}">${label}</button>`;
}
function usagePeriod(view) {
  const {report,period,date}=view;
  const daily=period==='day';
  const earliest=daily ? date<=report.first_day : report.start<=usageMonday(report.first_day);
  const latest=daily ? date>=report.today : report.end>report.today;
  const label=daily ? usageDateLabel(date)+' · '+usageWeekdays[usageDate(date).getUTCDay()] :
    usageDateLabel(report.start)+' – '+usageDateLabel(usageShift(report.end,-1));
  return `<div class="usage-period"><div class="usage-date">
    <button class="icon-button" type="button" data-action="usage-step" data-value="-1" aria-label="${daily?'前一天':'上一周'}" ${earliest?'disabled':''}>${icon('chevron-right')}</button>
    <b>${label}</b>
    <button class="icon-button" type="button" data-action="usage-step" data-value="1" aria-label="${daily?'后一天':'下一周'}" ${latest?'disabled':''}>${icon('chevron-right')}</button>
    </div><div class="usage-segment" role="group" aria-label="查看周期">${usageChoice('按周','usage-period','week',!daily)}${usageChoice('按天','usage-period','day',daily)}</div></div>`;
}

function usageChart(view) {
  const {report,period}=view;
  const daily=period==='day', names=daily ? usageHours : usageWeekdays.slice(1).concat(usageWeekdays[0]);
  const data=names.map((label,index)=>({label,date:daily?'':usageShift(report.start,index),cost:0}));
  const buckets=new Map(data.map(bucket=>[bucket.date,bucket]));
  for (const row of report.entries) {
    const bucket=daily ? data[Math.floor(Number(row.started_at.slice(11,13))/6)] : buckets.get(row.day);
    bucket.cost+=row.cost_micros;
  }
  // 旧账有日期，所以可进入周图；日视图的时段只用有实际时间的记录。
  if (!daily) for (const row of report.legacy) buckets.get(row.day).cost+=row.cost_micros;
  const max=Math.max(1,...data.map(bucket=>bucket.cost));
  const columns=data.map(({label,date,cost})=>{
    const future=!daily && date>report.today;
    const amount=future ? '—' : usageMoney(cost);
    const mark=`<b>${amount}</b><i style="--height:${cost/max*70}px" aria-hidden="true"></i><span>${label}</span>`;
    const css=`usage-column${cost===max?' is-peak':''}`;
    return daily ? `<div class="${css}">${mark}</div>` :
      `<button class="${css}" type="button" data-action="usage-day" data-value="${date}" aria-label="${usageDateLabel(date)} ${label}${future?'，未到该日期':'，花费 '+amount}" ${future?'disabled':''}>${mark}</button>`;
  }).join('');
  const plot=daily && report.legacy.length && !report.entries.length ?
    '<p class="usage-notice">这天的旧账只有当日合计。</p>' :
    `<div class="usage-chart${daily?' is-day':''}" role="group" aria-label="估算花费，单位美元">${columns}</div>`;
  return `<section><h2 class="usage-title">${daily?'各时段花费':'每天花费'}</h2>${plot}${daily && report.legacy.length && report.entries.length?'<p class="usage-muted">时段图按有时间的调用绘制，旧账计入当天合计。</p>':''}</section>`;
}

function usageMatterName(row) {
  const excerpt=row.matter.replace(/\s+/g,' ').trim();
  if (excerpt) return excerpt;
  if (row.root_kind==='research' && row.conversation) return row.conversation+' · 研究';
  return row.conversation || (row.root_id?'个人活动':row.purpose==='preferences'?'偏好设置':'系统调用');
}
function usageIdentity(row,group) {
  if (group==='member') return [row.member_id || (row.conversation_id?'shared':'system'),row.member_name || (row.member_id?'成员':row.conversation_id?'会话共用':'系统共用')];
  if (group==='purpose') return [row.purpose,usagePurposes[row.purpose] || '模型调用'];
  return [row.root_id || row.conversation_id || (row.purpose==='preferences'?'preferences':'system'),usageMatterName(row)];
}
function usageAvatar(id) {
  const person=member(id);
  return person ? avatar(person,true) : `<span class="avatar small usage-shared" aria-hidden="true">${icon('users')}</span>`;
}
function usageCategories(entries,group) {
  const categories=new Map();
  for (const row of entries) {
    const [id,name]=usageIdentity(row,group);
    const total=categories.get(id) || {id,name,cost:0};
    total.cost+=row.cost_micros;
    categories.set(id,total);
  }
  return [...categories.values()].sort((a,b)=>b.cost-a.cost || a.name.localeCompare(b.name,'zh-CN'));
}
function usageBreakdown(view) {
  const group=view.group;
  const totals=usageCategories(view.report.entries,group).filter(item=>item.cost>0);
  const max=totals[0]?.cost || 1;
  const tabs=Object.entries(usageGroups).map(([key,label])=>usageChoice('按'+label,'usage-group',key,group===key)).join('');
  const rows=totals.map(({id,name,cost})=>`<button class="usage-rank" type="button" data-action="usage-rank" data-value="${escapeHTML(id)}" aria-pressed="${view.filters[group]===id}" aria-label="筛选${usageGroups[group]}${escapeHTML(name)}，花费 ${usageMoney(cost)}">
    <span class="usage-rank-name">${group==='member'?usageAvatar(id):''}<span>${escapeHTML(name)}</span></span>
    <span class="usage-mini" aria-hidden="true"><i style="width:${cost/max*100}%"></i></span><b>${usageMoney(cost)}</b></button>`).join('');
  return `<section class="usage-section"><div class="usage-section-head"><h2 class="usage-title">钱花在哪儿</h2><div class="usage-segment" role="group" aria-label="按什么查看花费">${tabs}</div></div>
    <div class="usage-ranking">${rows || '<p class="usage-notice">这个时段没有可分类的花费。</p>'}</div></section>`;
}

function usageRecord(row,daily,open) {
  const settled=row.status==='settled';
  const amount=settled ? usageMoney(row.cost_micros,row.cost_micros>=10000?3:undefined) : usageMoney(row.reserved_micros);
  const status=settled ? usageOutcomes[row.outcome] || '已完成' : row.status==='running'?'处理中，额度暂时占用':'结果待核实，额度暂时占用';
  const kind=usagePurposes[row.purpose] || '模型调用';
  const owner=usageIdentity(row,'member')[1], matter=usageMatterName(row);
  const time=(daily?'':usageDateLabel(row.day)+' ')+row.started_at.slice(11,16);
  const tokens=n=>n.toLocaleString('zh-CN');
  const usage=!settled ? '等待核实' : row.provider==='DeepSeek' ? '1 次外部请求' :
    `输入 ${tokens(row.input_tokens)} · 输出 ${tokens(row.output_tokens)} tokens${row.cached_tokens?'<br>缓存读取 '+tokens(row.cached_tokens):''}${row.cache_write_tokens?'<br>缓存写入 '+tokens(row.cache_write_tokens):''}${row.reasoning_tokens!=null?'<br>其中推理 '+tokens(row.reasoning_tokens)+' tokens':''}`;
  const room=state.conversations.find(c=>c.id===row.conversation_id);
  const link=room ? `<button type="button" class="usage-pill" data-action="usage-room" data-value="${escapeHTML(room.id)}">打开原会话 ${icon('chevron-right')}</button>` : '';
  return `<details class="usage-record" data-record="${row.id}" ${open?'open':''}><summary>${usageAvatar(row.member_id)}
    <span class="usage-record-label"><strong>${kind}${settled?'':` <span class="usage-pending">· ${row.status==='running'?'处理中':'待核实'}</span>`}</strong><small>${escapeHTML(time+' · '+owner+' · '+matter)}</small></span>
    <b>${amount}</b>${icon('chevron-down')}</summary>
    <div class="usage-record-body"><dl><div><dt>模型 / 工具</dt><dd>${escapeHTML(row.model)}</dd></div><div><dt>用量</dt><dd>${usage}</dd></div><div><dt>结果</dt><dd>${status}</dd></div>
    ${row.autonomous?'<div><dt>来源</dt><dd>成员自主行动</dd></div>':''}</dl>${link}</div></details>`;
}
function usageLedger(view,open) {
  const {entries,legacy}=view.report;
  const groups=Object.entries(usageGroups);
  const active=groups.filter(([key])=>view.filters[key]);
  const filtered=entries.filter(row=>active.every(([key])=>usageIdentity(row,key)[0]===view.filters[key]));
  const all=Object.fromEntries(groups.map(([key])=>[key,usageCategories(entries,key)]));
  const filters=groups.map(([key,label])=>{
    // 切日期后仍可辨认已选条件；这个时段没有匹配时，筛选不会静默清空。
    const id=view.filters[key];
    if (id && !all[key].some(item=>item.id===id)) all[key].unshift({id,name:view.filterNames[key]});
    const options=all[key].map(({id,name})=>`<option value="${escapeHTML(id)}" ${view.filters[key]===id?'selected':''}>${escapeHTML(name)}</option>`).join('');
    return `<div class="usage-filter-field"><label for="usage-filter-${key}">${label}</label><select id="usage-filter-${key}" data-usage-filter="${key}"><option value="">不限</option>${options}</select></div>`;
  }).join('');
  const chips=active.map(([key,label])=>{
    const name=all[key].find(item=>item.id===view.filters[key])?.name || label;
    return `<button class="usage-pill" type="button" data-action="usage-clear" data-value="${key}" aria-label="移除${label}筛选 ${escapeHTML(name)}">${escapeHTML(name)} ${icon('x')}</button>`;
  }).join('');
  const counts=`${filtered.length} 笔调用${active.length ? ' · 花费约 '+usageMoney(usageSum(filtered))+(usageSum(filtered,'reserved_micros')?' · 暂占 '+usageMoney(usageSum(filtered,'reserved_micros')):'') : ''}`;
  const old=!active.length && legacy.length ? `<div class="usage-history"><h3>历史汇总</h3><p class="usage-muted">这部分旧账只记录到天，已计入上方合计。</p>
    ${legacy.map(row=>`<div class="usage-history-day"><span>${usageDateLabel(row.day)} · ${row.calls} 笔</span><span>${usageMoney(row.cost_micros)}${row.reserved_micros?' · 暂占 '+usageMoney(row.reserved_micros):''}</span></div>`).join('')}</div>` : '';
  return `<section class="usage-section"><div class="usage-section-head"><h2 class="usage-title">逐笔明细</h2><button class="usage-pill" type="button" data-action="usage-filters" aria-expanded="${view.showFilters}" aria-controls="usage-filters">筛选 ${icon('chevron-down')}</button></div>
    <div class="usage-filters" id="usage-filters" ${view.showFilters?'':'hidden'}>${filters}</div>
    ${chips?`<div class="usage-chips" aria-label="当前筛选">${chips}</div>`:''}
    <p class="usage-muted usage-ledger-count" aria-live="polite">${counts}</p>
    ${filtered.slice(0,view.limit).map(row=>usageRecord(row,view.period==='day',open.has(String(row.id)))).join('')}
    ${filtered.length>view.limit?'<button type="button" class="usage-pill usage-more" data-action="usage-more">查看更多明细</button>':''}
    ${!filtered.length && !old?`<p class="usage-notice">${active.length?'这组条件下没有调用记录。':'这段时间还没有调用记录。'}</p>`:''}${old}</section>`;
}

function renderUsageAnalysis(view,focusSelector='') {
  const root=view.root, active=document.activeElement;
  const focused=root.contains(active);
  const action=focused && active.dataset.action;
  const record=focused && active.closest('[data-record]')?.dataset.record;
  const filter=focused && active.dataset.usageFilter;
  if (!focusSelector && action) focusSelector=`[data-action="${CSS.escape(action)}"]${active.dataset.value?`[data-value="${CSS.escape(active.dataset.value)}"]`:''}`;
  if (!focusSelector && filter) focusSelector=`[data-usage-filter="${CSS.escape(filter)}"]`;
  if (!focusSelector && record) focusSelector=`[data-record="${record}"] summary`;
  const scroller=$('workspace-content'), scrollTop=scroller.scrollTop;
  const open=new Set([...root.querySelectorAll('details[open]')].map(item=>item.dataset.record));
  const error=view.error?'<div class="usage-error" role="alert"><span>没能读取额度记录。</span><button class="usage-pill" type="button" data-action="usage-retry">重试</button></div>':'';
  if (!view.report) { root.innerHTML=error; return; }
  const {entries,legacy}=view.report;
  const used=usageSum(entries)+usageSum(legacy);
  const running=usageSum(entries.filter(row=>row.status==='running'),'reserved_micros');
  const waiting=usageSum(entries.filter(row=>row.status==='unknown'),'reserved_micros')+usageSum(legacy,'reserved_micros');
  const reserved=running && waiting ? `处理中 ${usageMoney(running)} · 待核实 ${usageMoney(waiting)}` :
    running ? '请求处理中' : waiting ? '结果待核实' : '';
  root.innerHTML=error+usagePeriod(view)+`<div class="usage-summary" aria-live="polite"><span>${view.period==='day'?'这天花了':'这周花了'}（估算）</span><strong>${usageMoney(used)}</strong>
    ${reserved?`<p class="usage-muted">另有 ${usageMoney(running+waiting)} 暂占额度 · ${reserved}</p>`:''}</div>`+usageChart(view)+usageBreakdown(view)+usageLedger(view,open);
  const target=focusSelector && root.querySelector(focusSelector);
  scroller.scrollTop=scrollTop;
  if (target && !target.disabled) target.focus({preventScroll:true});
  else if (focused) root.querySelector('[data-action="usage-period"][aria-pressed="true"]')?.focus({preventScroll:true});
}

document.addEventListener('click',event=>{
  const button=event.target.closest('button[data-action^="usage-"]');
  if (!button) return;
  const {action,value}=button.dataset;
  if (action==='usage-open') {openUsageAnalysis();return;}
  if (action==='usage-back') {account();$('account-usage-entry').focus();return;}
  const view=usageView;
  if (!view?.root.contains(button)) return;
  if (action==='usage-room') {openRoom(value);return;}
  if (action==='usage-retry') {loadUsageAnalysis(view.request.period,view.request.date);return;}
  const {report,period,date}=view;
  if (action==='usage-period') {
    if (value!==period) loadUsageAnalysis(value,date<report.first_day?report.first_day:date);
    return;
  }
  if (action==='usage-day') {
    // 返回日视图后，键盘焦点落在按天按钮，原周柱已不在页面上。
    loadUsageAnalysis('day',value);
    return;
  }
  if (action==='usage-step') {
    const next=usageShift(date,Number(value)*(period==='week'?7:1));
    loadUsageAnalysis(period,next>report.today?report.today:next);
    return;
  }
  if (action==='usage-group') view.group=value;
  if (action==='usage-rank') {
    view.filters[view.group]=view.filters[view.group]===value?'':value;
    view.filterNames[view.group]=button.querySelector('.usage-rank-name').textContent.trim();
  }
  if (action==='usage-filters') view.showFilters=!view.showFilters;
  if (action==='usage-clear') delete view.filters[value];
  const nextRecord=action==='usage-more'?view.limit+1:0;
  view.limit=nextRecord ? view.limit+10 : action==='usage-rank'||action==='usage-clear' ? usagePageSize : view.limit;
  renderUsageAnalysis(view,nextRecord?`.usage-record:nth-of-type(${nextRecord}) summary`:action==='usage-clear'?'[data-action="usage-filters"]':'');
});
document.addEventListener('change',event=>{
  const key=event.target.dataset.usageFilter;
  if (!key || !usageView?.root.contains(event.target)) return;
  usageView.filters[key]=event.target.value;
  usageView.filterNames[key]=event.target.selectedOptions[0].textContent;
  usageView.limit=usagePageSize;
  renderUsageAnalysis(usageView);
});
