let currentLog = 'service.log';
let accounts = [];
let curAccount = null;

const $ = id => document.getElementById(id);

function fmtTime(){
  const d = new Date(), p = n => String(n).padStart(2,'0');
  $('clock').textContent = d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());
}
setInterval(fmtTime,1000); fmtTime();

// esc 把值转义成 HTML 文本。
//
// 注意：这里必须同时转义单引号。面板大量使用
//   onclick="runTask('"+esc(id)+"','daily')"
// 这种写法 —— 值落在**单引号**包裹的 JS 字符串里。早先只转义
// & < > "，如果 id 里含 ' 就能逃出字符串执行任意 JS。
// 当前 id 是手机号（纯数字）所以不可利用，但这是结构性缺陷：
// 一旦 id 来源变化（比如支持自定义别名）就会变成真实 XSS。
function esc(s){
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({
    '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
  }[c]));
}

// api 统一收口所有后端请求。
//
// 三个必须在这里处理的事情（原来都没有，导致会话过期后界面完全错乱）：
//   1. 认证失效：后端返回 401 或 body.need_login 时，立刻切回登录页。
//      不处理的话 loadAccounts 拿到 undefined.accounts → 显示
//      「还没有账号」，用户以为数据被清空了。
//   2. 超时：同步任务（Star / 活动报名）要 30-90 秒，原来没有超时，
//      卡住时用户只能刷新整页。
//   3. 非 JSON 响应：返回 HTML 错误页时给出可读提示，而不是 JSON 解析异常。
//
// 超时值必须 >= 后端最慢的同步任务。GitCode 账密登录后端给 180 秒
// （要开浏览器、切 tab、等风控），前端比它短的话会先断开，
// 用户看到「请求超时」以为失败，实际后台还在跑 —— 这就是之前的 120s bug。
const API_TIMEOUT_MS = 240000;

async function api(path, opts){
  const ctrl = new AbortController();
  // 允许单次调用覆盖超时（长任务用）
  const ms = (opts && opts.timeoutMs) || API_TIMEOUT_MS;
  const timer = setTimeout(() => ctrl.abort(), ms);
  let r;
  try{
    r = await fetch(path, Object.assign({credentials: 'same-origin'}, opts || {}, {signal: ctrl.signal}));
  }catch(e){
    clearTimeout(timer);
    if(e.name === 'AbortError'){
      throw new Error('请求超时（' + Math.round(ms/1000) + ' 秒），后台可能仍在执行，请刷新查看');
    }
    throw e;
  }
  clearTimeout(timer);

  const text = await r.text();
  let data;
  try { data = JSON.parse(text) }
  catch(e){
    // 后端返回了 HTML（404 兜底页、反代错误页）
    throw new Error('服务返回了非 JSON 内容（HTTP ' + r.status + '），可能是路由错误或服务异常');
  }

  // 认证失效 → 切回登录页，不要继续渲染主界面
  if(r.status === 401 || (data && data.need_login)){
    showAuthScreen((data && data.error) || '登录已过期，请重新登录');
    throw new Error('need_login');
  }
  if(data && data.need_setup){
    showAuthScreen('面板尚未初始化');
    throw new Error('need_setup');
  }
  if(data && data.broken){
    showAuthScreen('面板账号数据损坏，请在服务器上检查 state/panel_users.json');
    throw new Error('panel_broken');
  }

  return data;
}

// showAuthScreen 切回登录页并显示原因。
function showAuthScreen(reason){
  authState = {logged_in: false};
  const main = $('mainWrap');
  const auth = $('authScreen');
  if(main) main.style.display = 'none';
  if(auth) auth.style.display = '';
  const msg = $('authMsg');
  if(msg && reason){
    msg.textContent = reason;
    msg.className = 'msg show err';
  }
  const u = $('authUser');
  if(u) u.focus();
}

function showMsg(el, text, ok){
  const box = $(el);
  box.textContent = text;
  box.className = 'msg show ' + (ok ? 'ok' : 'err');
  if(ok) setTimeout(() => box.classList.remove('show'), 6000);
}

// ===== 认证 =====
let authState = {need_setup:false, logged_in:false, username:'', expire_at:''};

async function checkAuth(){
  try{
    const r = await api('/api/auth/state');
    authState = r;
    if(r.logged_in){
      showMain();
    }else if(r.need_setup){
      $('authTitle').textContent = '首次使用';
      $('authSub').textContent = '创建一个管理员账号，登录有效期 30 天';
      $('setupBlock').style.display = 'block';
      $('loginBlock').style.display = 'none';
      $('authBtn').textContent = '创建并进入';
      $('setupUser').focus();
    }else{
      $('authTitle').textContent = 'hwcloud 控制台';
      $('authSub').textContent = '请登录（有效期 30 天）';
      $('setupBlock').style.display = 'none';
      $('loginBlock').style.display = 'block';
      $('authBtn').textContent = '登录';
      $('authUser').focus();
    }
  }catch(e){
    showMsg('authMsg', e.message, false);
  }
}

function showMain(){
  $('authScreen').style.display = 'none';
  $('mainWrap').style.display = 'block';
  if(authState.username){
    $('userInfo').textContent = authState.username + (authState.expire_at ? ' · 有效期至 ' + authState.expire_at.slice(0,10) : '');
  }
  loadAccounts();
  loadTimers();
  loadLog(currentLog);
}

async function doAuth(){
  $('authBtn').disabled = true;
  try{
    if(authState.need_setup){
      const u = $('setupUser').value.trim();
      const p1 = $('setupPw').value, p2 = $('setupPw2').value;
      if(u.length < 3) return showMsg('authMsg', '用户名至少 3 位', false);
      if(p1.length < 6) return showMsg('authMsg', '密码至少 6 位', false);
      if(p1 !== p2) return showMsg('authMsg', '两次输入不一致', false);
      const r = await api('/api/auth/setup', {
        method:'POST', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({username:u, password:p1})
      });
      if(r.error) return showMsg('authMsg', r.error, false);
      authState = {need_setup:false, logged_in:true, username:u};
      showMain();
    }else{
      const u = $('authUser').value.trim();
      const pw = $('authPw').value;
      if(!u || !pw) return showMsg('authMsg', '请输入用户名和密码', false);
      const r = await api('/api/auth/login', {
        method:'POST', headers:{'Content-Type':'application/json'},
        body: JSON.stringify({username:u, password:pw})
      });
      if(r.error) return showMsg('authMsg', r.error, false);
      authState = {need_setup:false, logged_in:true, username:u};
      showMain();
    }
  }catch(e){
    showMsg('authMsg', e.message, false);
  }finally{
    $('authBtn').disabled = false;
  }
}

async function doLogout(){
  try{ await api('/api/auth/logout', {method:'POST'}) }catch(e){}
  location.reload();
}

// 回车提交
document.addEventListener('keydown', e => {
  if(e.key === 'Enter' && $('authScreen').style.display !== 'none'){
    doAuth();
  }
});

// ===== 账号列表 =====
async function loadAccounts(){
  try{
    const d = await api('/api/accounts');
    accounts = d.accounts || [];
    $('accCount').textContent = accounts.length;

    if(!accounts.length){
      $('accList').innerHTML = '<div class="empty">还没有账号，点击右上角「添加账号」</div>';
      return;
    }

    $('accList').innerHTML = accounts.map(a => renderAccount(a)).join('');

    // 后台补查绑定状态：有 GitCode 用户名但还没查过的，悄悄查一次。
    //
    // 为什么要自动查：用户设完 GitCode 账密后，界面上「GitCode: xxx」
    // 看起来一切正常，但实际上可能根本没绑定到华为云，Issue 白提。
    // 让用户自己想起来去点「查绑定状态」不现实，所以这里自动补。
    //
    // 串行 + 延迟，避免刚开页面就并发打一堆请求把会话检查挤爆。
    setTimeout(autoProbeBindStates, 1200);
  }catch(e){
    // 认证失效已由 api() 处理（切回登录页），这里不覆盖列表。
    if(e.message === 'need_login' || e.message === 'need_setup' || e.message === 'panel_broken'){
      return;
    }
    // 其他错误（网络抖动、超时）保留上一次的账号列表，只在消息区提示。
    // 原来是把整块列表替换成一行红字，用户正在看的账号信息瞬间消失，
    // 而且计数还停在旧值，界面自相矛盾。
    showMsg('globalMsg', '刷新账号列表失败：' + e.message, false);
  }
}

// gitcodeBtn 渲染 GitCode 按钮，按绑定状态给出不同文案和样式。
//
// 为什么不能只显示「GitCode: 用户名」：
//   那只能说明本地存了用户名，**不代表华为云认了这个绑定**。
//   没绑定时提 Issue 不计分，但界面看起来一切正常，用户根本不知道。
//
// 五种状态：
//   ""        未查询 —— 普通「绑定GitCode」
//   bound     已绑定 —— 绿色 ✓
//   unbound   未绑定 —— 黄色 ⚠，最需要用户注意
//   mismatch  绑定的号和存的不一致 —— 黄色 ⚠
//   error     查询失败（会话失效）—— 灰色
function gitcodeBtn(a){
  const id = esc(a.id);
  const u = a.gitcode_user ? esc(a.gitcode_user) : '';
  const st = a.gitcode_bind_state || '';
  const msg = a.gitcode_bind_msg ? esc(a.gitcode_bind_msg) : '';

  let label, cls, title;
  switch(st){
    case 'bound':
      label = '✓ GitCode: ' + u;
      cls = ' ok';
      title = '已绑定到华为云，Issue / Star 会计入任务';
      break;
    case 'unbound':
      label = '⚠ GitCode 未绑定';
      cls = ' warn';
      title = msg || 'GitCode 账号没有绑定到华为云，Issue / Star 不会计入成长中心';
      break;
    case 'mismatch':
      label = '⚠ GitCode 账号不一致';
      cls = ' warn';
      title = msg;
      break;
    case 'error':
      label = u ? 'GitCode: ' + u : '绑定GitCode';
      cls = ' dim';
      title = msg || '绑定状态查询失败';
      break;
    default:
      label = u ? 'GitCode: ' + u : '绑定GitCode';
      cls = '';
      title = u ? '已存用户名，尚未查询绑定状态' : '设置 GitCode 账号并绑定到华为云';
  }
  return '<button class="btn sm' + cls + '" data-act="gitcode" data-arg="' + id +
         '" title="' + title + '">' + label + '</button>';
}

// autoProbeBindStates 给「有 GitCode 用户名但没查过绑定状态」的账号补查一次。
//
// 只查状态未知或过期的（超过 6 小时），避免每次刷新页面都打接口。
// 串行执行，失败静默 —— 这是锦上添花的信息，不该因为查不到就弹错误。
let bindProbeRunning = false;
async function autoProbeBindStates(){
  if(bindProbeRunning) return;
  bindProbeRunning = true;
  try{
    const stale = accounts.filter(a => {
      if(!a.gitcode_user) return false;
      const st = a.gitcode_bind_state || '';
      if(st === '') return true;                  // 从没查过
      // 查询失败的重试，但别太频繁
      if(st === 'error' && !a.gitcode_bind_at) return true;
      return false;
    });
    if(!stale.length) return;

    let changed = false;
    for(const a of stale){
      try{
        const r = await api('/api/accounts/gitcode/bindstate?id=' + encodeURIComponent(a.id),
                            {timeoutMs: 45000});
        if(r && r.state && r.state !== a.gitcode_bind_state){
          a.gitcode_bind_state = r.state;
          a.gitcode_bind_msg = r.msg || '';
          a.gitcode_bind_at = r.at || '';
          changed = true;
        }
      }catch(_){
        // 单个账号查失败不影响其他账号
      }
    }
    if(changed){
      $('accList').innerHTML = accounts.map(a => renderAccount(a)).join('');
    }
  }finally{
    bindProbeRunning = false;
  }
}

// remarkChip 渲染备注徽标。没备注时显示一个淡淡的「+备注」提示，
// 让用户知道这个位置可以点。
function remarkChip(a){
  const id = esc(a.id);
  if(a.remark){
    return '<span class="acc-remark" data-act="remark" data-arg="' + id +
           '" title="点击修改备注">' + esc(a.remark) + '</span>';
  }
  return '<span class="acc-remark empty" data-act="remark" data-arg="' + id +
         '" title="给这个账号加个备注">+ 备注</span>';
}

// editRemark 编辑账号备注。
//
// 用 prompt 不行（artifact 环境里被禁用），所以直接在卡片上方
// 插一个输入行，回车保存、Esc 取消。
function editRemark(id){
  // 用属性选择器 + 转义双引号，不用 CSS.escape（老浏览器没有这个 API）。
  // 账号 id 是手机号，不会含引号，但转义一下更稳。
  const safe = String(id).replace(/"/g, '\\"');
  const card = document.querySelector('.acc[data-id="' + safe + '"]');
  if(!card){
    showMsg('globalMsg', '找不到账号卡片，请刷新页面', false);
    return;
  }
  if(card.querySelector('.remark-editor')) return;  // 已经开着

  const a = accounts.find(x => x.id === id);
  const head = card.querySelector('.acc-head');
  if(!head) return;

  const wrap = document.createElement('div');
  wrap.className = 'remark-editor';
  wrap.innerHTML =
    '<input class="remark-input" maxlength="40" placeholder="给这个账号起个名字，比如「主号」「测试」「项目A」">' +
    '<button class="btn sm primary" data-r="save">保存</button>' +
    '<button class="btn sm" data-r="cancel">取消</button>';
  head.parentElement.insertBefore(wrap, head.nextSibling);

  const input = wrap.querySelector('.remark-input');
  input.value = (a && a.remark) || '';
  input.focus();
  input.select();

  const close = () => wrap.remove();

  const save = async () => {
    const val = input.value.trim();
    try{
      const r = await api('/api/accounts/remark', {
        method: 'POST', headers: {'Content-Type':'application/json'},
        body: JSON.stringify({id, remark: val})
      });
      if(r.error) throw new Error(r.error);
      if(a) a.remark = r.remark || '';
      close();
      $('accList').innerHTML = accounts.map(x => renderAccount(x)).join('');
    }catch(e){
      showMsg('globalMsg', '保存备注失败：' + e.message, false);
    }
  };

  wrap.addEventListener('click', ev => {
    const b = ev.target.closest('[data-r]');
    if(!b) return;
    if(b.dataset.r === 'save') save();
    else close();
  });
  input.addEventListener('keydown', ev => {
    if(ev.key === 'Enter'){ ev.preventDefault(); save() }
    if(ev.key === 'Escape'){ ev.preventDefault(); close() }
  });
  // 失焦不自动关闭 —— 用户可能只是点到别处看一眼
}

function renderAccount(a){
  const alive = a.alive === true;
  const status = alive
    ? '<span class="dot ok"></span><span class="ok">在线</span>'
    : '<span class="dot err"></span><span class="err">离线</span>';
  const trust = a.trusted
    ? '<span class="ok">已信任</span>'
    : '<span class="warn">未信任</span>';

  const items = [
    ['状态', status],
    ['设备信任', trust],
  ];
  if(a.username) items.push(['用户名', esc(a.username)]);
  // 积分：-1 表示取不到（离线/接口失败），显示 — 而不是 0
  items.push(['积分', a.beans >= 0
    ? '<span class="beans">' + a.beans + '</span>'
    : '<span class="hint">—</span>']);
  if(a.remain_days > 0) items.push(['凭据剩余', a.remain_days.toFixed(1) + ' 天']);
  if(a.last_sign) items.push(['上次签到', esc(a.last_sign.slice(5,16))]);
  if(a.last_devspace) items.push(['上次DevSpace', esc(a.last_devspace.slice(5,16))]);
  if(a.last_error) items.push(['错误', '<span class="err">' + esc(a.last_error.slice(0,40)) + '</span>']);

  // 按钮统一用 data-act / data-arg 传参，由容器上的事件委托分发。
  //
  // 为什么不用 onclick="runTask('...')"：
  //   HTML 实体转义（&#39;）在属性解析时会被**解码回 '**，
  //   所以在 onclick 里拼 JS 字符串时，esc() 根本挡不住引号逃逸 ——
  //   实测 <button onclick="runTask('x&#39;);alert(1);//')"> 会变成
  //   onclick="runTask('x');alert(1);//')"，直接执行。
  //   data-* 属性的值只当数据读，不参与 JS 解析，天然安全。
  const act = (action, arg, label, cls) =>
    '<button class="btn sm' + (cls ? ' ' + cls : '') +
    '" data-act="' + action + '" data-arg="' + esc(arg) + '">' + label + '</button>';

  return '<div class="acc" data-id="' + esc(a.id) + '">' +
    '<div class="acc-head">' +
      // 备注放最前面，比手机号更醒目 —— 账号多了以后用户靠备注认号
      remarkChip(a) +
      '<span class="acc-phone">' + esc(a.phone) + '</span>' +
      (a.username ? '<span class="acc-user">' + esc(a.username) + '</span>' : '') +
      '<span class="spacer"></span>' +
      status +
    '</div>' +
    '<div class="acc-body">' +
      items.map(([k,v]) => '<div class="item"><span class="k">'+k+'</span><span>'+v+'</span></div>').join('') +
    '</div>' +
    '<div class="acc-actions">' +
      act('run', 'daily', '一键日常', 'primary') +
      act('run', 'weekly', '一键周常', 'primary') +
      act('run', 'sign', '签到') +
      act('run', 'devspace', 'DevSpace') +
      act('run', 'lab', '实验') +
      act('run', 'blog', '发文章') +
      act('run', 'issue', '提Issue') +
      act('run', 'star', 'Star') +
      act('run', 'eventjoin', '活动报名') +
      gitcodeBtn(a) +
      act('run', 'keepalive', '保活') +
      act('relogin', '', '重新登录') +
      act('check', '', '检查') +
      act('tasks', '', '任务') +
      '<span class="spacer"></span>' +
      act('remove', '', '删除', 'danger') +
    '</div>' +
  '</div>';
}

// ===== 全局 data-call 委托 =====
//
// 静态按钮统一写 data-call="fnName"，由这里分发到实际函数。
// 这样整页没有内联 onclick，CSP 才有机会收紧（虽然当前没上 CSP，
// 但去掉内联处理器是前置条件）。
//
// loadLogTab 是特例：它需要知道是哪个 tab 被点了，从 data-log 读。
const DATA_CALL = {
  doAuth, loadAccounts, openAdd, doLogout,
  saveGitCode, closeGitCode, closeTasks,
  doAdd, closeAdd, doLogin, doTrust, closeLogin,
  hideVnc, copyVncTunnel, copyVncPass,
  probeBindState,
  loadLogRefresh: () => loadLog(currentLog),
  loadLogTab: (el) => {
    const name = el.dataset.log;
    if(name) loadLog(name, el);
  },
};

function bindDataCalls(){
  if(document.body.dataset.callBound === '1') return;
  document.body.dataset.callBound = '1';
  document.body.addEventListener('click', (ev) => {
    const el = ev.target.closest('[data-call]');
    if(!el || el.disabled) return;
    const fn = DATA_CALL[el.dataset.call];
    if(typeof fn === 'function') fn(el);
  });
}

// ===== 账号卡片事件委托 =====
//
// 卡片里的按钮不带 onclick，只用 data-act / data-arg 存数据。
// 这里在 #accList 上统一监听，按 data-act 分发。
//
// 安全要点：data-arg 的值通过 dataset 读出来是**普通字符串**，
// 不经过任何 JS 解析，所以账号 id 里含引号、分号、括号都无害。
// 这是替代 onclick 拼接的正确做法 —— HTML 实体转义在 onclick
// 上下文里会被解码，挡不住引号逃逸。
function bindAccountActions(){
  const list = $('accList');
  if(!list || list.dataset.bound === '1') return;
  list.dataset.bound = '1';

  list.addEventListener('click', (ev) => {
    // 注意：这里不能用 button[data-act] —— 备注徽标是 <span>，
    // 用 button 限定会让它点不动。统一用 [data-act]，
    // 具体元素类型由渲染方决定。
    const btn = ev.target.closest('[data-act]');
    if(!btn || btn.disabled) return;
    const act = btn.dataset.act;
    const arg = btn.dataset.arg || '';
    // 账号 id 从所属卡片上取（卡片一定有 data-id）
    const card = btn.closest('.acc');
    const id = card ? card.getAttribute('data-id') : '';

    switch(act){
      case 'run':
        // arg 是任务名，id 来自卡片
        runTask(id, arg);
        break;
      case 'gitcode': openGitCode(arg || id); break;
      case 'remark': editRemark(arg || id); break;
      // 重新登录：用服务器上存的账号密码重走一遍完整登录流程
      // （滑块自动过、短信验证码照常填），不需要用户再输密码。
      case 'relogin':  startRelogin(id); break;
      case 'check':    checkAccount(id); break;
      case 'tasks':    showTasks(id); break;
      case 'remove':   removeAccount(id); break;
    }
  });
}

// ===== 添加账号 =====
function openAdd(){ $('addModal').classList.add('show'); $('newPhone').focus() }
function closeAdd(){ $('addModal').classList.remove('show'); $('addMsg').classList.remove('show') }

async function doAdd(){
  const phone = $('newPhone').value.trim();
  const password = $('newPassword').value;
  if(!phone || !password) return showMsg('addMsg', '请填写手机号和密码', false);
  showMsg('addMsg', '正在验证账号密码…', true);
  try{
    const r = await api('/api/accounts/add', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({phone, password})
    });
    if(r.error || r.ok === false){
      return showMsg('addMsg', r.error || '添加失败', false);
    }

    $('newPhone').value = ''; $('newPassword').value = '';
    closeAdd();

    // 待定账号：密码已验过，直接进入浏览器登录，成功后才入库
    const id = (r.account && r.account.id) || phone;
    pendingAccount = {id: id, phone: phone, password: password};
    openLogin(id, password);
    showMsg('loginMsg', '密码正确，正在启动浏览器登录…', true);
    // 直接开跑，用户不用再点一次「登录」
    setTimeout(() => doLogin(), 300);
  }catch(e){
    showMsg('addMsg', e.message, false);
  }
}

// startRelogin 用服务器上已保存的账号密码重走一遍登录流程。
//
// 跟「添加账号」的区别只有两点：
//   1. 不用输密码 —— 前端不传 password，后端从 account.json 取
//   2. 账号已在库里，登录成功不会重复入库，只是覆盖 state 里的 cookie
//
// 其余完全一样：Camoufox 开浏览器、自动过易盾滑块、需要时发短信、
// 用户在弹窗里填验证码。这样「会话彻底失效但保活救不回来」的账号
// 能一键重新登录，不用删了再加。
async function startRelogin(id){
  const a = accounts.find(x => x.id === id);
  if(!a) return;
  if(!a.password_saved){
    // 后端不下发密码，用这个布尔值判断有没有存
    showMsg('globalMsg', a.phone + ' 没有保存密码，请用「添加账号」重新添加', false);
    return;
  }
  openLogin(id, '', true);
  showMsg('loginMsg', '正在用已保存的密码登录…', true);
  // 直接开跑，用户不用再点一次「登录」
  setTimeout(() => doLogin(), 200);
}

async function removeAccount(id){
  if(!confirm('确定删除账号 ' + id + ' 及其所有数据？')) return;
  try{
    const r = await api('/api/accounts/remove', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({id})
    });
    if(r.error) return showMsg('globalMsg', r.error, false);
    showMsg('globalMsg', '已删除', true);
    loadAccounts();
  }catch(e){
    showMsg('globalMsg', e.message, false);
  }
}

// ===== 登录弹窗 =====
let cscState = null;       // 当前 csc 参数
let cscObj = null;         // 华为验证码实例
let cscStarting = false;   // 防止并发/重复初始化
let cscPopupShown = false; // 防止 onReady 多次触发导致叠层
let cscSubmitting = false; // 防止重复提交（会形成死循环）

let pendingAccount = null;  // 待定账号：已验密码但还没登录成功
// loginUseStored=true 表示这次登录用服务器上存的密码，前端不碰密码。
// 密码只存在于 account.json（0600），浏览器端从来没有拿到过。
let loginUseStored = false;

// openLogin 打开登录弹窗。
//
// 三种来源：
//   1. 「添加账号」——密码刚验过，presetPassword 传进来，直接复用
//   2. 「重新登录」——账号已入库，用服务器上存的密码，不显示密码框
//   3. 老路径——手动点，自己填手机号和密码
//
// useStored=true 时不把密码填进输入框（前端根本拿不到密码，
// 后端 /api/accounts/camoufox/login 只传 id 就会用 store 里的那份），
// 同时隐藏手机号和密码两个输入框 —— 避免用户误以为要自己再输一遍。
function openLogin(id, presetPassword, useStored){
  curAccount = accounts.find(a => a.id === id);
  // 待定账号还不在列表里，用 pendingAccount 兜底
  if(!curAccount && pendingAccount && pendingAccount.id === id){
    curAccount = {id: id, phone: pendingAccount.phone, trusted: false, _pending: true};
  }
  if(!curAccount) return;

  loginUseStored = !!useStored;

  $('loginTitle').textContent = (useStored ? '重新登录 ' : '登录 ') + curAccount.phone;
  $('loginSub').textContent = useStored
    ? '使用已保存的账号密码重新登录；需要时按提示填短信验证码'
    : '密码 + 短信验证码；浏览器在服务器上运行，会自动生成可信设备指纹';

  // useStored 时整块隐藏，只留进度和验证码
  const credStyle = useStored ? 'none' : '';
  $('loginPhoneLabel').style.display = credStyle;
  $('loginPhone').style.display = credStyle;
  $('loginPwdLabel').style.display = credStyle;
  $('loginPassword').style.display = credStyle;

  $('loginPhone').value = curAccount.phone || curAccount.id || '';
  // 从「添加账号」进来时密码已经填过，直接复用，不用再输一遍
  $('loginPassword').value = useStored ? '' : (presetPassword || '');
  $('loginCode').value = '';
  $('cscBlock').style.display = 'none';
  $('cscBox').innerHTML = '<div class="csc-hint" id="cscHint">正在启动浏览器…</div>';
  $('smsBlock').style.display = 'none';
  $('btnLogin').style.display = 'inline-block';
  $('btnLogin').disabled = false;
  $('btnLogin').textContent = '登录';
  $('btnTrust').style.display = 'none';
  if(smsTimer){ clearInterval(smsTimer); smsTimer = null; }
  $('loginMsg').classList.remove('show');
  cscState = null;
  cscStarting = false;
  cscPopupShown = false;
  try{ if(cscObj && typeof cscObj.destroy === 'function') cscObj.destroy(); }catch(e){}
  cscObj = null;
  clearCscOverlays();
  $('loginModal').classList.add('show');
  if(!useStored) $('loginPassword').focus();
}
// closeLogin 关闭登录弹窗。
//
// 【必须通知后端释放浏览器】
//
// Camoufox 是单实例服务，一个账号开着的浏览器会一直占着 LOCK 和
// 那 1-2 GB 内存。用户关了弹窗（尤其是「等短信但不想等了」的情况），
// 浏览器不会自己退出 —— 实测两个账号这么挂着，后面所有登录请求
// 都在 LOCK 上排队，最终报「连接 Camoufox 服务失败」，
// 而用户完全不知道是自己没填验证码导致的。
//
// 所以关闭时主动调 /logout 让服务端 drop_session → close()。
// 用 keepalive:true 是为了关页面时请求也能发出去。
function closeLogin(){
  $('loginModal').classList.remove('show');

  // 释放服务端会话。不阻塞 UI，失败也不弹错 ——
  // 用户点「关闭」就是想立刻脱身，不该被一个清理请求拖住。
  const id = curAccount && curAccount.id;
  if(id){
    api('/api/accounts/camoufox/logout', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({id: id})
    }).catch(() => {});
  }

  cscState = null;
  cscStarting = false;
  cscPopupShown = false;
  try{ if(cscObj && typeof cscObj.destroy === 'function') cscObj.destroy(); }catch(e){}
  cscObj = null;
  clearCscOverlays();
  if(smsTimer){ clearInterval(smsTimer); smsTimer = null; }
}

function loginCreds(){
  return {
    id: curAccount.id,
    phone: $('loginPhone').value.trim(),
    password: $('loginPassword').value,
    code: $('loginCode').value.trim(),
    useStored: loginUseStored,
  };
}

// 清理网易易盾残留的浮层（重复初始化会叠多层黑遮罩）
function clearCscOverlays(){
  document.querySelectorAll('.yidun_popup, .yidun_modal, .yidun_cover-frame')
    .forEach(el => { try{ el.remove(); }catch(e){} });
  document.querySelectorAll('#NECaptchaSafeWindow').forEach(el => {
    try{ el.remove(); }catch(e){}
  });
  // 恢复页面滚动（遮罩会加 overflow:hidden）
  try{ document.body.style.overflow = ''; }catch(e){}
}

// 加载华为验证码 SDK。
// 用 fetch + eval 而不是 <script src>：某些浏览器会把同源脚本按 ORB 规则拦掉。
async function loadCscSDK(){
  if(window.initHuaweiCaptcha) return;
  const r = await fetch('/api/accounts/csc/sdk.js');
  if(!r.ok) throw new Error('SDK 下载失败 HTTP ' + r.status);
  const code = await r.text();
  (0, eval)(code);
  if(!window.initHuaweiCaptcha) throw new Error('SDK 加载后没有 initHuaweiCaptcha');
}

// 拉滑块参数并在弹窗里渲染
async function startCsc(){
  // 防重入：已经在初始化就直接返回，避免叠出多个浮层
  if(cscStarting){
    return;
  }
  cscStarting = true;
  cscPopupShown = false;

  // 清掉上一轮的残留
  clearCscOverlays();
  try{ if(cscObj && typeof cscObj.destroy === 'function') cscObj.destroy(); }catch(e){}
  cscObj = null;

  const box = $('cscBox');
  box.innerHTML = '<div class="csc-hint" id="cscHint">正在获取验证参数…</div>';
  $('cscBlock').style.display = 'block';

  try{
    const r = await api('/api/accounts/csc?id=' + encodeURIComponent(curAccount.id));
    if(r.error || r.stage !== 'csc'){
      box.innerHTML = '<div class="csc-hint err">' + esc(r.message || r.error || '获取失败') + '</div>';
      cscStarting = false;
      return;
    }
    cscState = r;

    await loadCscSDK();

    box.innerHTML = '<div class="csc-hint">请在弹出的验证窗口中拖动滑块完成拼图</div>';

    const cfg = {
      appId: r.app_id,
      businessId: r.business_id,
      sceneId: r.scene_id,
      type: r.type,
      challenge: r.challenge,
      hcg: r.hcg,
      hct: r.hct,
      serviceDomain: r.service_domain || [],
      width: '320px',
      lang: 'zh-CN',
      isCloseable: false,
      protocol: 'https',
      timeout: 15000,
      onReady: () => {
        // onReady 可能被触发多次，只弹一次
        if(cscPopupShown) return;
        cscPopupShown = true;
        showMsg('loginMsg', '请拖动滑块完成验证', true);
        try{
          if(cscObj && typeof cscObj.popupShow === 'function') cscObj.popupShow();
        }catch(e){ console.error('popupShow:', e); }
      },
    };

    cscObj = window.initHuaweiCaptcha(
      cfg,
      () => {
        // onLoad 兜底：某些 type 下 onReady 不触发，这里再试一次
        if(cscPopupShown) return;
        cscPopupShown = true;
        showMsg('loginMsg', '请拖动滑块完成验证', true);
        try{
          if(cscObj && typeof cscObj.popupShow === 'function') cscObj.popupShow();
        }catch(e){ console.error('popupShow onLoad:', e); }
      },
      (e) => {
        cscStarting = false;
        // SDK 成功时回调 {type, resultCode:0, resultMsg, lang, challenge, hcg, hct, validate, ...}
        // 注意：初始化阶段也可能回调一次（此时没有 validate），必须过滤掉，
        // 否则会拿空 validate 去提交，失败后又重开滑块 —— 死循环。
        const v = e && typeof e.validate === 'string' ? e.validate : '';
        if(e && e.resultCode === 0 && v.length > 0){
          console.log('csc onVerify validate len=' + v.length);
          submitCsc(e);
        }else if(e && e.resultCode === 0){
          console.log('csc onVerify 无 validate，忽略（初始化回调）');
        }else{
          showMsg('loginMsg', '验证未通过，请重试', false);
        }
      },
      () => { cscStarting = false; showMsg('loginMsg', '验证窗口已关闭', false); },
      (e) => { cscStarting = false; showMsg('loginMsg', '验证出错: ' + ((e && e.errorMsg) || JSON.stringify(e)), false); }
    );

    // 兜底：3 秒后如果还没弹过，强制弹一次
    setTimeout(() => {
      if(cscPopupShown) return;
      cscPopupShown = true;
      showMsg('loginMsg', '请拖动滑块完成验证', true);
      try{
        if(cscObj && typeof cscObj.popupShow === 'function') cscObj.popupShow();
      }catch(e){ console.error('popupShow fallback:', e); }
    }, 3000);
  }catch(e){
    cscStarting = false;
    box.innerHTML = '<div class="csc-hint err">' + esc(e.message) + '</div>';
  }
}

// 拿到 validate 后提交，完成登录。
// 传完整回调对象：后端会从中取 challenge/hcg/hct/validate，保证与滑块那次完全一致。
async function submitCsc(cb){
  if(cscSubmitting) return;      // 防重入，避免失败后连锁重试
  cscSubmitting = true;

  const validate = (cb && cb.validate) || '';
  showMsg('loginMsg', '验证通过，正在登录…', true);
  try{
    const r = await api('/api/accounts/csc/submit', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({id: curAccount.id, validate: validate, callback: cb || null})
    });
    // 登录结果交给统一处理器；失败时不会自动重开滑块
    handleLoginResult(r, true);
  }catch(e){
    showMsg('loginMsg', e.message, false);
  }finally{
    cscSubmitting = false;
  }
}

// 统一处理登录结果。
// fromCsc=true 表示这是滑块提交后的结果：失败时只提示，绝不自动重开滑块
// （否则会形成「失败 → 重开 → 立刻回调 → 再失败」的死循环）。
function handleLoginResult(r, fromCsc){
  if(r.error) return showMsg('loginMsg', r.error, false);

  if(r.stage === 'done'){
    showMsg('loginMsg', r.message || '登录成功', true);
    pendingAccount = null;
    setTimeout(() => { closeLogin(); loadAccounts() }, 1200);
    return;
  }

  if(r.need_sms){
    // 滑块已通过，立刻关掉验证弹窗；短信由后端自动发送
    try{ if(cscObj && typeof cscObj.destroy === 'function') cscObj.destroy(); }catch(e){}
    cscObj = null;
    cscPopupShown = false;
    clearCscOverlays();

    showMsg('loginMsg', r.message || '验证通过，短信已发送，请填写验证码', true);
    $('smsBlock').style.display = 'block';
    $('btnTrust').style.display = 'inline-block';
    $('btnLogin').style.display = 'none';
    $('loginCode').focus();
    return;
  }

  if(fromCsc){
    // 滑块已提交过但登录仍失败：显示原因，让用户手动点「重新验证」
    const ec = r.error_code ? ('[' + r.error_code + '] ') : '';
    showMsg('loginMsg', ec + (r.message || '登录失败'), false);
    // 给一个手动重试入口
    $('btnLogin').style.display = 'inline-block';
    $('btnLogin').textContent = '重新验证';
    $('btnLogin').disabled = false;
    return;
  }

  // 风控要求滑块验证（仅首次，非滑块提交后）
  if(r.error_code === '10000706' || r.error_code === '10000201' || r.need_manual){
    showMsg('loginMsg', r.message || '需要安全验证', false);
    startCsc();
    return;
  }

  showMsg('loginMsg', r.message || JSON.stringify(r), false);
}

// 登录：已信任设备走纯账密；未信任的直接走滑块验证。
// 浏览器登录：调服务器上的 Camoufox，用真实 Firefox 生成设备指纹。
// 它比纯协议稳的原因：fp 是每次会话动态生成的，纯协议复用旧值会被判「设备不安全」。
async function doLogin(){
  const c = loginCreds();
  // useStored 时前端没有密码（也不该有），交给后端用 account.json 里的那份。
  if(!c.password && !c.useStored) return showMsg('loginMsg', '请填写密码', false);

  $('btnLogin').disabled = true;
  $('btnLogin').style.display = 'none';
  $('cscBlock').style.display = 'block';
  $('cscHint').textContent = '正在启动浏览器并提交账号密码…';
  showMsg('loginMsg', '浏览器登录中，约 20-40 秒…', true);

  try{
    // useStored 时不传 password 字段 —— 后端看到 id 会自己去 store 里取。
    // 这样密码永远不会出现在浏览器内存或网络请求里。
    const body = c.useStored
      ? {id: curAccount.id, phone: c.phone}
      : {id: curAccount.id, phone: c.phone, password: c.password};

    const r = await api('/api/accounts/camoufox/login', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify(body)
    });

    if(r.error){ throw new Error(r.error); }

    if(r.stage === 'need_sms'){
      $('smsBlock').style.display = 'block';
      $('btnTrust').style.display = 'inline-block';
      $('loginCode').value = '';
      $('loginCode').focus();
      startSmsCountdown(r.ttl || 480);
      showMsg('loginMsg', '验证码已发送到手机，请查收', true);
      return;
    }

    if(r.stage === 'done'){
      $('cscHint').textContent = '登录成功';
      showMsg('loginMsg', '登录成功', true);
      closeLogin();
      await loadAccounts();
      return;
    }

    throw new Error(r.error || ('登录未完成：' + (r.stage || 'unknown')));
  }catch(e){
    $('cscBlock').style.display = 'none';
    $('btnLogin').style.display = 'inline-block';
    showMsg('loginMsg', e.message, false);
  }finally{
    $('btnLogin').disabled = false;
  }
}

// 短信倒计时：过期后提示重新点登录（点登录会强制重发，不再复用旧会话）
let smsTimer = null;
function startSmsCountdown(ttl){
  if(smsTimer) clearInterval(smsTimer);
  let left = ttl;
  const tick = () => {
    if(left <= 0){
      clearInterval(smsTimer); smsTimer = null;
      $('cscHint').textContent = '验证码已过期';
      showMsg('loginMsg', '验证码已过期，请重新点「登录」获取新验证码', false);
      $('btnTrust').style.display = 'none';
      $('btnLogin').style.display = 'inline-block';
      $('btnLogin').textContent = '重新获取验证码';
      return;
    }
    const m = Math.floor(left / 60), sec = left % 60;
    $('cscHint').textContent = '验证码已发送，剩余 ' + m + ':' + String(sec).padStart(2,'0');
    left--;
  };
  tick();
  smsTimer = setInterval(tick, 1000);
}

// 提交短信验证码，完成浏览器登录
async function doTrust(){
  const c = loginCreds();
  if(!c.code) return showMsg('loginMsg', '请填写验证码', false);
  $('btnTrust').disabled = true;
  $('cscHint').textContent = '正在提交验证码…';
  try{
    // 填码阶段同样不带密码：后端按 id 复用 Camoufox 那边已开的会话，
    // 会话里已经存着手机号，密码用不上。
    const r = await api('/api/accounts/camoufox/login', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({id: curAccount.id, phone: c.phone, sms_code: c.code})
    });
    if(r.error){ throw new Error(r.error); }
    if(r.stage === 'done'){
      $('cscHint').textContent = '登录成功';
      showMsg('loginMsg', '登录成功', true);
      closeLogin();
      await loadAccounts();
      return;
    }
    throw new Error('验证码未通过：' + (r.error || r.stage));
  }catch(e){
    showMsg('loginMsg', e.message, false);
  }finally{
    $('btnTrust').disabled = false;
  }
}

// ===== 任务 =====
// 任务名 → 中文标签。放这里而不是函数后面，避免 const 的 TDZ 问题。
const TASK_LABELS = {
  daily: '一键日常', weekly: '一键周常', sign: '签到', devspace: 'DevSpace',
  lab: '云实验', blog: '发文章', issue: '提Issue', star: 'Star', eventjoin: '活动报名', keepalive: '保活',
};

// runningTasks 记录正在执行的任务，防止连点。
//
// 后端 startJob 对 daily/weekly 有并发保护，但其他任务（Star、活动报名、
// 提Issue）没有 —— 连点会真的起两次浏览器，互相踢登录态。
// 这里在前端按「账号+任务」加锁，按钮变灰并显示进度。
const runningTasks = new Set();

async function runTask(id, name){
  const label = TASK_LABELS[name] || name;
  const key = id + ':' + name;
  if(runningTasks.has(key)){
    showMsg('globalMsg', label + ' 正在执行中，请等待完成…', false);
    return;
  }
  runningTasks.add(key);
  lockTaskButtons(id, true);

  const started = Date.now();
  // 同步任务（Star / 活动报名 / 提Issue）要 30-90 秒，只有一个静态提示
  // 会让用户以为卡死。这里每秒刷新已用时长。
  const tick = setInterval(() => {
    const s = Math.round((Date.now() - started) / 1000);
    showMsg('globalMsg', label + ' 执行中… 已用 ' + s + ' 秒', true);
  }, 1000);

  showMsg('globalMsg', label + ' 执行中，请稍候…', true);
  try{
    const r = await api('/api/accounts/run?id=' + encodeURIComponent(id) + '&name=' + encodeURIComponent(name), {method: 'POST'});
    if(r.error) return showMsg('globalMsg', label + ' 失败: ' + r.error, false);

    // 长任务（日常/周常）是异步的：立即返回 job，后台跑。
    // 这里轮询 /api/jobs 直到结束，期间把步骤打到日志区，
    // 避免用户以为「点了没反应」。
    if(r.async && r.job){
      showMsg('globalMsg', r.job.label + ' 已在后台执行，进度见日志区…', true);
      switchLog('service.log');
      pollJob(r.job.id, r.job.label);
      return;
    }

    // 一键任务返回 {ok,label,msg,steps}，逐条展示每一步的结果
    if(r.steps && r.steps.length){
      const lines = r.steps.map(st => (st.ok ? '✓ ' : '✗ ') + st.name + '：' + (st.msg || ''));
      showMsg('globalMsg', lines.join('　'), r.ok);
      return loadAccounts();
    }

    // 单步任务：把结果整理成人话，别把原始 JSON 甩给用户
    const elapsed = Math.round((Date.now() - started) / 1000);
    showMsg('globalMsg', label + ' 完成（' + elapsed + ' 秒）' + summarizeResult(r), true);
    loadAccounts();
  }catch(e){
    showMsg('globalMsg', label + ' 失败: ' + e.message, false);
  }finally{
    clearInterval(tick);
    runningTasks.delete(key);
    lockTaskButtons(id, false);
  }
}

// summarizeResult 把接口返回压成一句人话。
// 原始 JSON 直接 slice 给用户看没有意义（"完成: {"ok":true,"repo":"..."}"）。
function summarizeResult(r){
  if(!r || typeof r !== 'object') return '';
  if(r.ok === false) return '：' + (r.msg || r.result || r.error || '未完成');
  const bits = [];
  if(r.repo) bits.push(r.repo);
  if(r.url) bits.push(r.url);
  if(r.activity) bits.push('活动 ' + String(r.activity).slice(0, 8));
  if(r.title) bits.push(r.title);
  if(r.result) bits.push(r.result);
  if(r.msg) bits.push(r.msg);
  return bits.length ? '：' + bits.join('，') : '';
}

// lockTaskButtons 把某账号卡片上的任务按钮整体置灰/恢复。
//
// CSS.escape 在很老的浏览器里没有，用 getAttribute 比对兜底 ——
// 账号 id 是手机号，但支持自定义后可能含特殊字符，不能直接拼选择器。
function lockTaskButtons(id, locked){
  let acc = null;
  if(typeof CSS !== 'undefined' && CSS.escape){
    acc = document.querySelector('.acc[data-id="' + CSS.escape(id) + '"]');
  }
  if(!acc){
    acc = Array.from(document.querySelectorAll('.acc'))
      .find(e => e.getAttribute('data-id') === id) || null;
  }
  if(!acc) return;
  acc.querySelectorAll('button').forEach(b => {
    // 删除按钮始终可用，其余在任务执行期间禁用
    if(locked && !/删除/.test(b.textContent)) b.disabled = true;
    if(!locked) b.disabled = false;
  });
}

// pollJob 轮询后台任务状态，结束后展示结果。
//
// 用 Map 而不是单个全局 timer：原来 pollTimer 是单例，起第二个后台任务
// 时会 clearInterval 掉第一个的轮询 —— 第一个任务就此失去进度显示，
// 而且它的 runningTasks 锁也不会被释放（按钮永久变灰）。
// 现在每个 jobId 一个 timer，互不干扰。
const pollTimers = new Map();

function pollJob(jobId, label){
  // 同一个 job 重复调用时先清掉旧的
  if(pollTimers.has(jobId)){
    clearInterval(pollTimers.get(jobId));
    pollTimers.delete(jobId);
  }
  const started = Date.now();
  const timer = setInterval(async () => {
    const stop = () => {
      clearInterval(timer);
      pollTimers.delete(jobId);
      // 任务结束后释放对应的按钮锁（key 是 账号:任务名，这里拿不到账号，
      // 由 runTask 的 finally 负责；此处只清理轮询）
    };
    // 页面在后台时不轮询，省流量也避免无意义的日志重传
    if(document.hidden) return;

    try{
      const d = await api('/api/jobs?id=' + encodeURIComponent(jobId));
      if(d.error){ stop(); return; }
      const j = d.job;
      const elapsed = Math.round((Date.now() - started) / 1000);

      if(j.running){
        showMsg('globalMsg', label + ' 执行中…（' + j.step + '，已 ' + elapsed + ' 秒）', true);
        return;
      }

      stop();
      if(j.error){
        showMsg('globalMsg', label + ' 失败: ' + j.error, false);
      }else if(j.result && j.result.steps){
        const steps = j.result.steps;
        const lines = steps.map(st => (st.ok ? '✓ ' : '✗ ') + st.name + '：' + (st.msg || ''));
        showMsg('globalMsg', label + ' ' + j.result.msg + '　' + lines.join('　'), j.result.ok);
      }else{
        showMsg('globalMsg', label + ' 完成（' + elapsed + ' 秒）', true);
      }
      loadAccounts();
    }catch(e){
      stop();
      showMsg('globalMsg', '查询任务状态失败: ' + e.message, false);
    }
  }, 3000);
  pollTimers.set(jobId, timer);
}

// resumeRunningJobs 页面加载时把还在跑的后台任务重新接上进度显示。
//
// 原来刷新页面后，正在跑的任务就彻底看不见了 —— /api/jobs 本来就支持
// 返回全部运行中的任务，只是前端从来没调用。
async function resumeRunningJobs(){
  try{
    const d = await api('/api/jobs');
    const running = (d.jobs || []).filter(j => j.running);
    if(!running.length) return;
    for(const j of running){
      if(!pollTimers.has(j.id)) pollJob(j.id, j.label || j.name);
    }
    showMsg('globalMsg', '有 ' + running.length + ' 个后台任务正在执行，已接上进度显示', true);
  }catch(e){ /* 拿不到就算了，不打扰用户 */ }
}


async function checkAccount(id){
  try{
    const r = await api('/api/accounts/check?id=' + encodeURIComponent(id));
    // stale=true 表示锁被占、拿到的是缓存。必须如实告诉用户，
    // 否则会把「有任务在跑」误读成「账号离线」。
    if(r.stale){
      showMsg('globalMsg', '暂时无法检查：' + (r.stale_reason || '有任务正在执行'), false);
      return;
    }
    if(r.alive === null || r.alive === undefined){
      showMsg('globalMsg', '检查失败：拿不到会话状态', false);
      return;
    }
    showMsg('globalMsg', '检查完成: ' + (r.alive ? '在线' : '离线') + (r.username ? ' / ' + r.username : ''), r.alive);
    loadAccounts();
  }catch(e){
    showMsg('globalMsg', e.message, false);
  }
}

// 拉取成长中心的完整任务列表（1-5 星全查）。
// 之前 CLI 默认只查 1,2 星，3/4 星的任务（微认证、发作品、赛事）根本看不到。
async function showTasks(id){
  showMsg('globalMsg', '正在拉取任务列表…', true);
  try{
    const r = await api('/api/accounts/run?id=' + encodeURIComponent(id) + '&name=tasks', {method: 'POST'});
    if(r.error) return showMsg('globalMsg', r.error, false);
    const list = r.tasks || [];
    if(!list.length) return showMsg('globalMsg', '没有任务', false);

    const done = list.filter(t => t.done).length;
    $('tasksTitle').textContent = '成长中心任务（' + done + '/' + list.length + ' 已完成）';
    const stars = {1:'★', 2:'★★', 3:'★★★', 4:'★★★★', 5:'★★★★★'};
    $('tasksBody').innerHTML = list.map(t => {
      const mark = t.done
        ? '<span style="color:var(--ok)">已完成</span>'
        : '<span style="color:var(--warn)">未完成</span>';
      return '<div class="task-row">' +
        '<div class="task-title">' + esc(t.title) + '</div>' +
        '<div class="task-meta">' +
          '<span>' + (stars[t.degree] || t.degree + '星') + '</span>' +
          '<span>+' + t.bonus + ' 积分</span>' +
          '<span>' + mark + '</span>' +
        '</div>' +
      '</div>';
    }).join('');
    $('tasksModal').classList.add('show');
    showMsg('globalMsg', '任务列表已加载', true);
  }catch(e){
    showMsg('globalMsg', e.message, false);
  }
}
function closeTasks(){ $('tasksModal').classList.remove('show') }

// ===== GitCode 凭据 =====
let gcAccountId = null;
function openGitCode(id){
  gcAccountId = id;
  const a = accounts.find(x => x.id === id);
  $('gcUser').value = (a && a.gitcode_user) || '';
  $('gcPass').value = '';
  $('gcMsg').classList.remove('show');
  $('gcVnc').hidden = true;   // 每次打开先收起，失败时再展开
  renderBindState(a);
  $('gitcodeModal').classList.add('show');
  $('gcUser').focus();
  // 有用户名但状态未知，打开弹窗时补查一次
  if(a && a.gitcode_user && !a.gitcode_bind_state) probeBindState();
}

// renderBindState 把绑定状态画到弹窗顶部的横幅上。
function renderBindState(a){
  const box = $('gcBindState');
  const st = (a && a.gitcode_bind_state) || '';
  if(!a || !a.gitcode_user){
    box.hidden = true;
    return;
  }
  box.hidden = false;
  box.className = 'bind-state';

  const icon = $('gcBindIcon'), text = $('gcBindText');
  switch(st){
    case 'bound':
      box.classList.add('ok');
      icon.textContent = '✓';
      text.textContent = '已绑定到华为云（' + a.gitcode_user + '），Issue / Star 会计入任务';
      break;
    case 'unbound':
      box.classList.add('warn');
      icon.textContent = '⚠';
      text.textContent = '未绑定！GitCode 账号没绑定到华为云，提 Issue / Star 都不计分';
      break;
    case 'mismatch':
      box.classList.add('warn');
      icon.textContent = '⚠';
      text.textContent = a.gitcode_bind_msg || '华为云绑定的 GitCode 账号与这里存的不一致';
      break;
    case 'error':
      box.classList.add('dim');
      icon.textContent = '?';
      text.textContent = a.gitcode_bind_msg || '绑定状态查询失败（华为云会话可能已失效）';
      break;
    default:
      box.classList.add('dim');
      icon.textContent = '•';
      text.textContent = '尚未查询绑定状态';
  }
}

// probeBindState 主动查一次绑定状态并刷新横幅。
async function probeBindState(btn){
  if(!gcAccountId) return;
  const a = accounts.find(x => x.id === gcAccountId);
  if(!a || !a.gitcode_user){
    showMsg('gcMsg', '请先填写并保存 GitCode 用户名', false);
    return;
  }
  if(btn) btn.disabled = true;
  $('gcBindIcon').textContent = '…';
  $('gcBindText').textContent = '正在查询绑定状态…';
  $('gcBindState').hidden = false;
  try{
    const r = await api('/api/accounts/gitcode/bindstate?id=' + encodeURIComponent(gcAccountId),
                        {timeoutMs: 45000});
    a.gitcode_bind_state = r.state || '';
    a.gitcode_bind_msg = r.msg || '';
    a.gitcode_bind_at = r.at || '';
    renderBindState(a);
    // 列表上的按钮也跟着变
    $('accList').innerHTML = accounts.map(x => renderAccount(x)).join('');
  }catch(e){
    $('gcBindState').className = 'bind-state dim';
    $('gcBindIcon').textContent = '?';
    $('gcBindText').textContent = '查询失败：' + e.message;
  }finally{
    if(btn) btn.disabled = false;
  }
}
function closeGitCode(){ $('gitcodeModal').classList.remove('show') }

// ===== VNC 提示 =====
// GitCode / 华为云登录可能卡在滑块验证码，浏览器跑在服务器的 Xvfb 上，
// 用户本地看不到画面。这里把连接方式直接摊在弹窗里，不用去翻文档。
let vncCache = null;

function hideVnc(){ $('gcVnc').hidden = true }

// showVnc 展开 VNC 面板。
//
// mode:
//   'running'  登录正在进行 —— 提前给用户入口，别让他干等 3 分钟
//   'stuck'    登录失败/卡住 —— 明确告诉用户需要人工操作
async function showVnc(mode){
  const box = $('gcVnc');
  box.hidden = false;

  // 标题和说明随阶段变化，避免「登录还没开始就说需要人工过验证码」的误导
  if(mode === 'running'){
    $('vncTitle').textContent = '登录进行中，可以在这里看画面';
    $('vncTip').textContent = '服务器正在打开浏览器。如果卡在滑块验证码，用下面任一方式连进来手动操作，不用一直等。';
  }else if(mode === 'stuck'){
    $('vncTitle').textContent = '需要人工过验证码';
    $('vncTip').textContent = '登录卡住了。连进来看画面，手动拖一下滑块即可。';
  }

  $('vncTunnel').textContent = '查询中…';
  $('vncAddr').textContent = '—';
  $('vncPass').textContent = '—';
  $('vncHint').textContent = '';
  $('vncWebRow').hidden = true;
  $('vncSep').hidden = true;

  try{
    // running 阶段每次重新取，因为刚点按钮时服务可能还没起来
    if(!vncCache || mode === 'running') vncCache = await api('/api/vnc');
    const d = vncCache;

    // 网页版优先：用户不用装客户端、不用开隧道，点一下就进
    if(d.web_available){
      $('vncWebRow').hidden = false;
      // 带上 password 参数，noVNC 会自动用它做凭据，不弹密码框。
      // 见 /usr/share/novnc/app/ui.js: password = WebUtil.getConfigVar('password')
      //
      // 其余参数：
      //   autoconnect=1  打开即连，不用再点「连接」
      //   resize=scale   画面自适应窗口
      //   reconnect=1    断线自动重连（浏览器任务重启时会断）
      let url = d.web_url || '/vnc/';
      const qs = [];
      if(d.password) qs.push('password=' + encodeURIComponent(d.password));
      qs.push('autoconnect=1', 'resize=scale', 'reconnect=1', 'reconnect_delay=2000');
      url += (url.indexOf('?') >= 0 ? '&' : '?') + qs.join('&');
      $('vncWebBtn').href = url;
      $('vncSep').hidden = false;
    }

    if(!d.available){
      $('vncTunnel').textContent = '—';
      $('vncHint').textContent = d.hint || 'VNC 不可用';
      return;
    }
    $('vncTunnel').textContent = d.tunnel_cmd || '—';
    $('vncAddr').textContent = d.connect_to || ('127.0.0.1:' + d.port);
    $('vncPass').textContent = d.password || '(未配置)';
    const hints = [];
    if(d.hint) hints.push(d.hint);
    if(d.web_hint) hints.push(d.web_hint);
    $('vncHint').textContent = hints.join('；');
  }catch(e){
    $('vncTunnel').textContent = '—';
    $('vncHint').textContent = '获取 VNC 信息失败：' + e.message;
  }
}

// 复制到剪贴板。navigator.clipboard 在非 HTTPS 下不可用，
// 退回选中文本让用户自己 Ctrl+C。
// copyText 复制到剪贴板，三层降级。
//
// 为什么需要三层：面板常部署在 http://<IP>:8899，而
// navigator.clipboard 只在**安全上下文**（HTTPS 或 localhost）可用。
// 用 IP + HTTP 访问时它是 undefined，直接调用会抛错 ——
// 用户看到的就是「点了复制没反应」。
//
//   1. navigator.clipboard  —— 首选，安全上下文
//   2. execCommand('copy')  —— 老 API，HTTP 下仍可用
//   3. 选中文本             —— 最后兜底，让用户自己 Ctrl+C
async function copyText(text, btn){
  if(!text || text === '—' || text === '查询中…') return;

  let ok = false;

  // 1. 现代 API
  if(navigator.clipboard && window.isSecureContext){
    try{
      await navigator.clipboard.writeText(text);
      ok = true;
    }catch(_){}
  }

  // 2. execCommand 兜底：用一个临时 textarea 装文本再选中复制。
  //    注意不能用 display:none，那样选不中。
  if(!ok){
    try{
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.setAttribute('readonly', '');
      ta.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
      document.body.appendChild(ta);
      ta.select();
      ta.setSelectionRange(0, ta.value.length);
      ok = document.execCommand('copy');
      document.body.removeChild(ta);
    }catch(_){ ok = false }
  }

  // 3. 全失败：选中页面上的文本，让用户手动复制
  if(!ok){
    try{
      const el = btn ? btn.parentElement.querySelector('.vnc-cmd') : null;
      if(el){
        const sel = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(el);
        sel.removeAllRanges();
        sel.addRange(range);
      }
    }catch(_){}
  }

  // 反馈：成功显示「已复制」，失败显示「请手动复制」
  if(btn){
    const old = btn.textContent;
    btn.textContent = ok ? '已复制' : '请手动复制';
    setTimeout(() => { btn.textContent = old }, 1600);
  }
}
function copyVncTunnel(btn){ copyText($('vncTunnel').textContent, btn) }
function copyVncPass(btn){ copyText($('vncPass').textContent, btn) }

async function saveGitCode(){
  const user = $('gcUser').value.trim();
  const pass = $('gcPass').value;
  if(!user || !pass) return showMsg('gcMsg', '请填写用户名和密码', false);

  const btn = event && event.target;
  if(btn) btn.disabled = true;

  try{
    // 1. 先存账密（这一步很快，几毫秒）
    showMsg('gcMsg', '正在保存账密…', true);
    const r1 = await api('/api/accounts/gitcode', {
      method:'POST', headers:{'Content-Type':'application/json'},
      body: JSON.stringify({id: gcAccountId, user, password: pass})
    });
    if(r1.error) throw new Error('保存账密失败: ' + r1.error);

    // 2. 登录 GitCode。
    //
    //    这步要开浏览器、可能要过滑块，实测 1-3 分钟。
    //    **在这里就把 VNC 入口摊开**，不要等超时或失败 ——
    //    用户很可能正卡在验证码上，等 3 分钟才知道能连进去太亏了。
    showMsg('gcMsg', '账密已保存，正在登录 GitCode…（1-3 分钟）下方可以直接看服务器画面', true);
    showVnc('running');   // 立刻展开，不 await（不阻塞主流程）

    const r2 = await api('/api/accounts/run?id=' + encodeURIComponent(gcAccountId) + '&name=gitcode', {method: 'POST'});

    if(r2.error){
      // 失败时把账密保存的结果也告诉用户，避免误以为全都没成
      throw new Error('账密已保存，但登录失败: ' + r2.error);
    }

    // 3. 登录成功后**必须绑定**。
    //
    //    这是之前的核心 bug：只登录不绑定，弹窗却说「登录成功」，
    //    用户以为搞定了，实际成长中心一分不给。
    //    登录（拿到 gitcode.com 会话）和绑定（华为云侧的关联记录）
    //    是两件完全不同的事。
    showMsg('gcMsg', '✓ GitCode 登录成功，正在绑定到华为云…', true);
    let bindOK = false, bindMsg = '', needUsername = false;
    try{
      const r3 = await api('/api/accounts/run?id=' + encodeURIComponent(gcAccountId) + '&name=bindgitcode',
                           {method: 'POST', timeoutMs: 120000});
      if(r3 && !r3.error && r3.bound === true){
        bindOK = true;
      }else{
        bindMsg = (r3 && r3.error) || '绑定未生效';
        needUsername = !!(r3 && r3.need_username);
      }
    }catch(be){
      bindMsg = be.message;
    }

    const who = r2.username || user;
    if(bindOK){
      showMsg('gcMsg', '✓ GitCode 登录并绑定成功：' + who, true);
      const a = accounts.find(x => x.id === gcAccountId);
      if(a){ a.gitcode_bind_state = 'bound'; a.gitcode_bind_msg = ''; renderBindState(a); }
      hideVnc();
      $('gcPass').value = '';
      loadAccounts();
      setTimeout(closeGitCode, 2500);
    }else{
      // 登录成功但绑定失败 —— 必须明确说出来，不能让用户以为全好了
      showMsg('gcMsg', '⚠ GitCode 登录成功，但绑定失败：' + bindMsg +
              '　（不绑定的话 Issue / Star 不计分）', false);
      const a = accounts.find(x => x.id === gcAccountId);
      if(a){ a.gitcode_bind_state = 'unbound'; a.gitcode_bind_msg = bindMsg; renderBindState(a); }
      // 用户名填错时，把输入框选中，用户直接改就行
      if(needUsername){
        $('gcUser').focus();
        $('gcUser').select();
      }
      $('gcPass').value = '';
      loadAccounts();
      showVnc('stuck');
    }
  }catch(e){
    showMsg('gcMsg', e.message, false);
    // 登录失败：十有八九是卡在滑块验证码，明确提示需要人工
    showVnc('stuck');
  }finally{
    if(btn) btn.disabled = false;
  }
}

// ===== 定时任务 / 日志 =====
async function loadTimers(){
  try{
    const d = await api('/api/timers');
    if(d.platform === 'windows'){
      $('timerBox').innerHTML = '<div class="hint">' + esc(d.note || 'Windows 环境') + '</div>';
      return;
    }
    if(d.raw){
      $('timerBox').innerHTML = '<pre>' + esc(d.raw) + '</pre>';
      return;
    }
    $('timerBox').innerHTML = '<div class="hint">' + esc(d.error || '无数据') + '</div>';
  }catch(e){
    $('timerBox').innerHTML = '<div class="hint err">' + esc(e.message) + '</div>';
  }
}

async function loadLog(name, btn){
  currentLog = name;
  document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.log === name));
  await refreshLog();
}

// switchLog 切到指定日志 tab 并立即刷新。
function switchLog(name){
  currentLog = name;
  document.querySelectorAll('.tab').forEach(t => t.classList.toggle('active', t.dataset.log === name));
  refreshLog();
}

// refreshLog 只重读当前日志，不动 tab 选中状态（供定时器调用）。
// 之前日志不会自动更新，必须手点「刷新」—— 用户看的是定时任务日志，
// 却看到几小时前的内容，会以为任务没跑。
async function refreshLog(){
  try{
    const d = await api('/api/logs?name=' + encodeURIComponent(currentLog));
    const lines = d.lines || [];
    const box = $('logBox');
    // 只有内容变化才重绘，避免每 5 秒把滚动位置弹回底部
    const text = lines.length ? lines.join('\n') : (d.error || '(空)');
    if(box.textContent !== text){
      const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 40;
      box.textContent = text;
      if(atBottom) box.scrollTop = box.scrollHeight;
    }
  }catch(e){
    $('logBox').textContent = e.message;
  }
}

// 事件委托必须在 DOM 就绪后、任何渲染之前绑定。
// 脚本放在 </body> 前，此时 DOM 已解析完，直接调用即可。
bindDataCalls();
bindAccountActions();

checkAuth().then(() => {
  // 刷新页面后把还在跑的后台任务接上进度显示
  if(authState.logged_in) resumeRunningJobs();
  if(authState.logged_in) applyURLParams();
});

// applyURLParams 处理地址栏里的深链接参数。
//
// 支持：
//   ?account=<账号ID或手机号>   登录后自动打开该账号的 GitCode 弹窗
//   &user=<GitCode用户名>       预填用户名
//   &pwd=<GitCode密码>          预填密码（可配合 auto=1 直接提交）
//   &auto=1                     自动点「保存并登录」
//
// 用途：给用户发一条链接就能直达某个账号的 GitCode 设置，
// 不用他先在列表里找账号、再点按钮、再手输账密。
//
// 安全说明：pwd 会出现在浏览器历史和 Referer 里，
// 所以处理完立刻用 history.replaceState 把地址栏擦干净。
function applyURLParams(){
  let q;
  try { q = new URLSearchParams(location.search) }
  catch(_){ return }

  const account = q.get('account');
  if(!account) return;

  // 账号 ID 可能是手机号，也可能带前缀；按 id 或 phone 都匹配
  const a = accounts.find(x =>
    x.id === account || x.phone === account ||
    x.id.endsWith(account) || x.phone.endsWith(account));
  if(!a){
    showMsg('globalMsg', '链接里的账号 ' + account + ' 不存在', false);
    return;
  }

  const user = q.get('user') || '';
  const pwd  = q.get('pwd') || '';
  const auto = q.get('auto') === '1';

  // 立刻把敏感参数从地址栏抹掉，避免留在历史记录里
  if(pwd || user || auto){
    try{
      const clean = new URLSearchParams(location.search);
      ['pwd','user','auto','account'].forEach(k => clean.delete(k));
      const qs = clean.toString();
      history.replaceState(null, '', location.pathname + (qs ? '?' + qs : ''));
    }catch(_){}
  }

  openGitCode(a.id);
  if(user) $('gcUser').value = user;
  if(pwd)  $('gcPass').value = pwd;

  if(auto && user && pwd){
    // 等一帧让弹窗渲染完再自动提交
    setTimeout(() => { saveGitCode(); }, 300);
  }
}

// 页面重新可见时立刻刷新一次，补上后台期间错过的更新
document.addEventListener('visibilitychange', () => {
  if(!document.hidden && authState.logged_in){
    loadAccounts();
    refreshLog();
    resumeRunningJobs();
  }
});
setInterval(() => {
  if(authState.logged_in) loadAccounts();
}, 45000);
// 日志自动刷新：5 秒一次，只刷新当前 tab
setInterval(() => {
  if(authState.logged_in) refreshLog();
}, 5000);
