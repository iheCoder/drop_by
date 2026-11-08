'use strict';

const usageWeekdays=['周日','周一','周二','周三','周四','周五','周六'];
const usageGroups={member:'成员',purpose:'用途',matter:'事项'};
const usagePurposes={chat:'日常交流',research:'研究与探索',personality:'人物整理',compact:'上下文压缩',search:'网页搜索',read_page:'网页读取',control:'指令理解',preferences:'偏好设置',model:'模型调用'};
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
  usageView={root:$('usage-analysis'),period:'day',date:'',group:'member',filters:{},filterNames:{},filterMenu:'',ascending:false,limit:usagePageSize,sequence:0};
  loadUsageAnalysis('day','');
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
    if (quiet && view.filterMenu) return;
    const same=quiet && JSON.stringify(view.report)===JSON.stringify(report);
    view.report=report;
    view.period=period;
    view.date=date || report.today;
    view.budget=budget;
    view.error=false;
    if (!quiet) {view.limit=usagePageSize;view.filterMenu='';}
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
  // SSE 刷新可更新在途用量；浏览历史或正在选择筛选项时保留当前画面。
  if (!view?.root.isConnected || !view.report || view.loading || view.error) return;
  const running=view.report.entries.some(row=>row.status==='running');
  if (!running && (view.report.end<=view.report.today || view.budget===JSON.stringify(state.usage))) return;
  if (view.filterMenu) return;
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
  const daily=period==='day';
  const max=Math.max(1,...report.trend.map(bucket=>bucket.cost_micros));
  const columns=report.trend.map(({day,calls,cost_micros:cost})=>{
    const active=day===report.start;
    const end=usageShift(day,6);
    const unavailable=!active && (daily ? day : end)<report.first_day;
    const amount=calls || cost ? usageMoney(cost) : '—';
    const label=daily ? usageDateLabel(day) : usageDateLabel(day)+' 起';
    const range=daily ? usageDateLabel(day)+' '+usageWeekdays[usageDate(day).getUTCDay()] :
      usageDateLabel(day)+' 至 '+usageDateLabel(end);
    return `<button class="usage-column" type="button" data-action="usage-bucket" data-value="${day}" aria-pressed="${active}"
      aria-label="${range}，${amount==='—'?'无记录':'花费 '+amount}" ${unavailable?'disabled':''}>
      <b>${amount}</b><i style="--height:${cost/max*70}px" aria-hidden="true"></i><span>${label}</span></button>`;
  }).join('');
  return `<section><div class="usage-section-head"><h2 class="usage-title">${daily?'每天花费':'每周花费'}</h2><span class="usage-muted">${daily?'每柱一天':'每柱一周 · 周一至周日'}</span></div>
    <div class="usage-chart" role="group" aria-label="估算花费，单位美元">${columns}</div></section>`;
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
function usageHistory(report,ascending) {
  const {entries,legacy}=report;
  if (!legacy.length) return '';
  const calls=usageSum(legacy,'calls');
  const days=[...legacy].sort((a,b)=>usageAmountOrder(a,b,!entries.length && ascending));
  return `<section class="usage-history" aria-label="历史汇总"><div class="usage-section-head"><h2>历史汇总 · ${calls.toLocaleString('zh-CN')} 笔调用</h2>
    ${!entries.length && days.length>1?usageSort(ascending):''}</div>
    <p class="usage-muted">${entries.length?'以下旧账已计入合计。':'这些费用已计入合计，当前时段还没有逐笔记录。'}旧账只记录到天。</p>
    ${days.map(row=>`<div class="usage-history-day"><span>${usageDateLabel(row.day)} · ${row.calls.toLocaleString('zh-CN')} 笔</span><span>${usageMoney(row.cost_micros)}${row.reserved_micros?' · 暂占 '+usageMoney(row.reserved_micros):''}</span></div>`).join('')}</section>`;
}
function usageAmountOrder(a,b,ascending) {
  // 已结算行按花费排序，待核实行按展示的暂占金额排序。相同金额保留原来的时间顺序。
  const difference=(a.cost_micros+a.reserved_micros)-(b.cost_micros+b.reserved_micros);
  return ascending?difference:-difference;
}
function usageSort(ascending) {
  return `<button class="usage-pill usage-sort" type="button" data-action="usage-sort" aria-label="金额排序，${ascending?'当前从低到高，点击切换为从高到低':'当前从高到低，点击切换为从低到高'}">金额${ascending?'从低到高':'从高到低'} ${icon(ascending?'arrow-up':'arrow-down')}</button>`;
}
function usageLedger(view,open) {
  const {entries,legacy}=view.report;
  const groups=Object.entries(usageGroups);
  const active=groups.filter(([key])=>view.filters[key]);
  const filtered=entries.filter(row=>active.every(([key])=>usageIdentity(row,key)[0]===view.filters[key]));
  filtered.sort((a,b)=>usageAmountOrder(a,b,view.ascending));
  const filters=groups.map(([key,label])=>{
    const expanded=view.filterMenu===key;
    const name=view.filters[key]?view.filterNames[key]:'不限';
    return `<button class="usage-filter" type="button" data-action="usage-filter" data-value="${key}" aria-expanded="${expanded}" aria-controls="usage-filter-options"
      aria-label="${label} ${escapeHTML(name)}" ${entries.length?'':'disabled'}><span>${label}</span><strong title="${escapeHTML(name)}">${escapeHTML(name)}</strong>${icon('chevron-down')}</button>`;
  }).join('');
  const key=view.filterMenu;
  let options='';
  if (key && entries.length) {
    const values=usageCategories(entries,key);
    const selected=view.filters[key] || '';
    if (selected && !values.some(item=>item.id===selected)) values.unshift({id:selected,name:view.filterNames[key]});
    values.unshift({id:'',name:'不限'});
    options=values.map(({id,name})=>usageChoice(escapeHTML(name),'usage-filter-option',id,selected===id)).join('');
  }
  const counts=entries.length ? `${filtered.length.toLocaleString('zh-CN')} 笔逐笔记录${active.length?' · 花费约 '+usageMoney(usageSum(filtered)):''}` :
    legacy.length?'新增调用会在这里显示成员、用途和事项。':'这段时间还没有调用记录。';
  return `<section class="usage-section"><div class="usage-section-head"><h2 class="usage-title">逐笔明细</h2>
    ${entries.length>1?usageSort(view.ascending):''}</div>
    <div class="usage-filters" aria-label="筛选逐笔明细">${filters}</div>
    <div class="usage-filter-options" id="usage-filter-options" role="group" aria-label="${usageGroups[key] || '筛选'}选项" ${options?'':'hidden'}>${options}</div>
    <p class="usage-muted usage-ledger-count" aria-live="polite">${counts}</p>
    ${filtered.slice(0,view.limit).map(row=>usageRecord(row,view.period==='day',open.has(String(row.id)))).join('')}
    ${filtered.length>view.limit?'<button type="button" class="usage-pill usage-more" data-action="usage-more">查看更多明细</button>':''}${active.length && entries.length && !filtered.length?'<p class="usage-notice">这组条件下没有匹配的调用。</p>':''}</section>`;
}

function renderUsageAnalysis(view,focusSelector='') {
  const root=view.root, active=document.activeElement;
  const focused=root.contains(active);
  const action=focused && active.dataset.action;
  const record=focused && active.closest('[data-record]')?.dataset.record;
  if (!focusSelector && action) focusSelector=`[data-action="${CSS.escape(action)}"]${active.dataset.value?`[data-value="${CSS.escape(active.dataset.value)}"]`:''}`;
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
    ${reserved?`<p class="usage-muted">另有 ${usageMoney(running+waiting)} 暂占额度 · ${reserved}</p>`:''}</div>`+usageChart(view)+usageHistory(view.report,view.ascending)+(entries.length?usageBreakdown(view):'')+usageLedger(view,open);
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
    if (value!==period) {
      const anchor=value==='day' && period==='week' ? usageShift(report.end,-1) : date;
      loadUsageAnalysis(value,anchor>report.today?report.today:anchor);
    }
    return;
  }
  if (action==='usage-bucket') {
    if (value!==report.start) loadUsageAnalysis(period,value);
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
  if (action==='usage-sort') view.ascending=!view.ascending;
  if (action==='usage-filter') {
    view.filterMenu=view.filterMenu===value?'':value;
    renderUsageAnalysis(view,view.filterMenu?'#usage-filter-options [aria-pressed="true"]':'');
    if (view.filterMenu) document.activeElement.scrollIntoView({block:'nearest'});
    return;
  }
  if (action==='usage-filter-option') {
    const key=view.filterMenu;
    view.filters[key]=value;
    view.filterNames[key]=button.textContent.trim();
    view.filterMenu='';
    view.limit=usagePageSize;
    renderUsageAnalysis(view,`[data-action="usage-filter"][data-value="${key}"]`);
    return;
  }
  const nextRecord=action==='usage-more'?view.limit+1:0;
  view.limit=nextRecord ? view.limit+10 : action==='usage-rank'||action==='usage-sort' ? usagePageSize : view.limit;
  renderUsageAnalysis(view,nextRecord?`.usage-record:nth-of-type(${nextRecord}) summary`:'');
});
document.addEventListener('click',event=>{
  const view=usageView;
  if (!view?.root.isConnected || !view.filterMenu || event.target.closest('.usage-filters,.usage-filter-options')) return;
  // 只收起选项，保留即将点击的行或控件，避免重绘吞掉用户的点击。
  view.root.querySelector('#usage-filter-options').hidden=true;
  view.root.querySelector('[data-action="usage-filter"][aria-expanded="true"]')?.setAttribute('aria-expanded','false');
  view.filterMenu='';
});
document.addEventListener('keydown',event=>{
  const view=usageView;
  if (event.key!=='Escape' || !view?.root.isConnected || !view.filterMenu) return;
  event.preventDefault();
  event.stopPropagation();
  const key=view.filterMenu;
  view.filterMenu='';
  renderUsageAnalysis(view,`[data-action="usage-filter"][data-value="${key}"]`);
},true);
