/* ===== 账户管理（ChatGPT + Codex 注册） ===== */
const ACC_STATUS = {
  pending: '待注册',
  registering: '注册中',
  registered: '已注册',
  register_failed: '注册失败',
  already_registered: '停用',
};
const CODEX_STATUS = {
  pending: '待授权',
  authorizing: '授权中',
  authorized: '已授权',
  failed: '授权失败',
};
const SUB2API_STATUS = {
  not_imported: '未导入',
  importing: '导入中',
  imported: '已导入',
  failed: '导入失败',
};
const AT_STATUS = {
  unchecked: '待检测', checking: '检测中', valid: '有效', invalid: '失效', error: '检测失败', missing: '无 AT',
};
const PLUS_MAIL_STATUS = {
  unchecked: '未检查', found: '已确认', not_found: '未发现', error: '检查失败',
};
const TRIAL_STATUS = {
  unchecked: '待检测', checking: '检测中', eligible: '有资格', ineligible: '无资格', error: '检测失败',
};
const TRIAL_PERIOD = {
  day: '天', week: '周', month: '个月', year: '年',
};
const PLAN_LABEL = {
  free: 'Free', go: 'Go', plus: 'Plus', pro: 'Pro', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu',
};
const COUNTRY_LABEL = {
  JP: '日本', US: '美国', DE: '德国', GB: '英国', KR: '韩国', FR: '法国', IT: '意大利',
  ES: '西班牙', NL: '荷兰', CA: '加拿大', AU: '澳大利亚', BR: '巴西', IN: '印度',
  ID: '印尼', VN: '越南', TH: '泰国', KH: '柬埔寨', SG: '新加坡', MY: '马来西亚',
  PH: '菲律宾', TW: '台湾', HK: '香港', CN: '中国', RU: '俄罗斯', TR: '土耳其',
  PL: '波兰', SE: '瑞典', NO: '挪威', DK: '丹麦', FI: '芬兰', CH: '瑞士',
  AT: '奥地利', BE: '比利时', IE: '爱尔兰', PT: '葡萄牙', MX: '墨西哥', AR: '阿根廷',
  CL: '智利', ZA: '南非', AE: '阿联酋', SA: '沙特', IL: '以色列', NZ: '新西兰',
};
let page = 1;
const size = 20;
let accCache = {};
let accTotal = 0;
let accountCategories = [];
let mailboxCategories = [];
let registerMailboxes = [];
let proxyPools = [];
let defaultProxyPoolID = 0;
const registerMailboxSelected = new Set();
const accSelected = new Set();
let plusMailChecking = false;

async function load() {
  const q = document.getElementById('search').value.trim();
  const status = document.getElementById('filter-status').value;
  const atStatus = document.getElementById('filter-at-status').value;
  const plan = document.getElementById('filter-plan').value;
  const trialStatus = document.getElementById('filter-trial-status').value;
  const registerCountry = document.getElementById('filter-register-country').value;
  const category = document.getElementById('filter-account-category').value;
  const params = new URLSearchParams({ page, size });
  if (q) params.set('q', q);
  if (status) params.set('status', status);
  if (atStatus) params.set('at_status', atStatus);
  if (plan) params.set('plan_type', plan);
  if (trialStatus) params.set('trial_status', trialStatus);
  if (registerCountry) params.set('register_country', registerCountry);
  if (category) params.set('category_id', category);
  const r = await api('/api/registrations?' + params);
  const d = await r.json();
  accCache = {};
  accTotal = d.total || 0;
  (d.data || []).forEach(x => { accCache[x.id] = x; });
  document.getElementById('rows').innerHTML = (d.data || []).map(rowHtml).join('')
    || '<tr><td colspan="13" style="text-align:center;color:var(--text-3)">暂无数据</td></tr>';
  syncCountryFilter(d.data || []);
  const maxPage = Math.max(1, Math.ceil((d.total || 0) / size));
  renderPager('pager', page, maxPage, p => { page = p; load(); });
  syncBatchBar();
}

function countryName(code) {
  const country = String(code || '').toUpperCase();
  return COUNTRY_LABEL[country] || country;
}

function registerCountryCell(x) {
  const country = String(x.register_country || '').toUpperCase();
  if (!country) return '<span class="table-muted">未知</span>';
  const title = [countryName(country) + ' ' + country, x.register_city || '', x.register_ip || ''].filter(Boolean).join(' · ');
  return `<span class="country-chip" title="${esc(title)}">${esc(countryName(country))}<span class="table-sub"> ${esc(country)}</span></span>`;
}

function syncCountryFilter(rows) {
  const select = document.getElementById('filter-register-country');
  if (!select) return;
  const current = select.value;
  const seen = new Set(Object.keys(COUNTRY_LABEL));
  rows.forEach(x => {
    const country = String(x.register_country || '').toUpperCase();
    if (country) seen.add(country);
  });
  const options = ['<option value="">全部注册地</option>', '<option value="unknown">注册地未知</option>'];
  [...seen].sort().forEach(code => {
    options.push(`<option value="${code}">${esc(countryName(code))}</option>`);
  });
  select.innerHTML = options.join('');
  if ([...select.options].some(option => option.value === current)) select.value = current;
}

function syncCountryFilter(rows) {
  const select = document.getElementById('filter-register-country');
  if (!select) return;
  const current = select.value;
  const seen = new Set(Object.keys(COUNTRY_LABEL));
  rows.forEach(x => {
    const country = String(x.register_country || '').toUpperCase();
    if (country) seen.add(country);
  });
  select.innerHTML = ['<option value="">全部注册地</option>', '<option value="unknown">注册地未知</option>']
    .concat([...seen].sort().map(code => `<option value="${code}">${esc(countryName(code))}</option>`))
    .join('');
  if ([...select.options].some(option => option.value === current)) select.value = current;
}

function registerCountryCell(x) {
  const country = String(x.register_country || '').toUpperCase();
  if (!country) return '<span class="table-muted">未知</span>';
  const title = [countryName(country) + ' ' + country, x.register_city || '', x.register_ip || ''].filter(Boolean).join(' · ');
  return `<span class="country-chip" title="${esc(title)}">${esc(countryName(country))}<span class="table-sub"> ${esc(country)}</span></span>`;
}

function syncCountryFilter(rows) {
  const select = document.getElementById('filter-register-country');
  if (!select) return;
  const current = select.value;
  const seen = new Set(Object.keys(COUNTRY_LABEL));
  rows.forEach(x => {
    const country = String(x.register_country || '').toUpperCase();
    if (country) seen.add(country);
  });
  const options = ['<option value="">全部注册地</option>', '<option value="unknown">注册地未知</option>'];
  [...seen].sort().forEach(code => {
    options.push(`<option value="${code}">${esc(countryName(code))}</option>`);
  });
  select.innerHTML = options.join('');
  if ([...select.options].some(option => option.value === current)) select.value = current;
}

function rowHtml(x) {
  const canDownload = x.status === 'registered';
  const canCopyAT = x.status === 'registered';
  const canAuthorizeCodex = x.status === 'registered' && x.codex_status !== 'authorizing';
  const canImportSub2API = x.status === 'registered' && x.codex_status === 'authorized' && x.sub2api_status !== 'importing';
  const codexTitle = x.codex_error ? esc(x.codex_error) : '';
  const sub2apiTitle = x.sub2api_error ? esc(x.sub2api_error) : (x.sub2api_account_id ? '远端账号 #' + x.sub2api_account_id : '');
  const atStatus = x.status === 'registered' ? (x.at_status || 'unchecked') : 'missing';
  const atTitle = [x.at_error, x.at_checked_at ? '最近检测：' + fmtTime(x.at_checked_at) : '', x.at_expires_at ? '到期：' + fmtTime(x.at_expires_at) : ''].filter(Boolean).join('\n');
  const plusMailStatus = x.plus_mail_status || 'unchecked';
  const plusMailTitle = [x.plus_mail_subject, x.plus_mail_error, x.plus_mail_received_at ? '邮件时间：' + fmtTime(x.plus_mail_received_at) : '', x.plus_mail_checked_at ? '检查时间：' + fmtTime(x.plus_mail_checked_at) : ''].filter(Boolean).join('\n');
  const trialStatus = x.trial_status || 'unchecked';
  const trialDuration = x.trial_periods && x.trial_period_unit ? `${x.trial_periods}${TRIAL_PERIOD[x.trial_period_unit] || x.trial_period_unit}` : '';
  const trialTitle = [
    x.trial_plan ? '套餐：' + (PLAN_LABEL[x.trial_plan] || x.trial_plan) : '',
    x.trial_percent ? '优惠：' + x.trial_percent + '%' : '',
    trialDuration ? '时长：' + trialDuration : '',
    trialStatus === 'eligible' ? '续订方式：' + (x.trial_auto_renew ? '自动续订' : '不自动续订') : '',
    x.trial_label ? '活动：' + x.trial_label : '',
    x.trial_error,
    x.trial_checked_at ? '检测时间：' + fmtTime(x.trial_checked_at) : '',
  ].filter(Boolean).join('\n');
  const plan = String(x.plan_type || '').toLowerCase();
  return `
    <tr class="${accSelected.has(x.id) ? 'row-sel' : ''}">
      <td class="col-check"><input type="checkbox" ${accSelected.has(x.id) ? 'checked' : ''} onclick="toggleSelect(${x.id}, this.checked)"></td>
      <td><div class="account-email">${esc(x.email)}</div><div class="table-sub">${fmtTime(x.created_at)}</div></td>
      <td>${x.category ? `<span class="category-chip">${esc(x.category.name)}</span>` : '<span class="table-muted">未分类</span>'}</td>
      <td>${registerCountryCell(x)}</td>
      <td><span class="badge at-${esc(atStatus)}" title="${esc(atTitle)}">${AT_STATUS[atStatus] || esc(atStatus)}</span></td>
      <td><span class="plan-badge plan-${esc(plan || 'unknown')}">${PLAN_LABEL[plan] || esc(plan || '未知')}</span></td>
      <td><span class="badge trial-${esc(trialStatus)}" title="${esc(trialTitle)}">${TRIAL_STATUS[trialStatus] || esc(trialStatus)}</span></td>
      <td><span class="badge ${plusMailStatus === 'found' ? 'registered' : (plusMailStatus === 'error' ? 'register_failed' : 'pending')}" title="${esc(plusMailTitle)}">${PLUS_MAIL_STATUS[plusMailStatus] || esc(plusMailStatus)}</span></td>
      <td><span class="badge ${esc(x.status)}">${ACC_STATUS[x.status] || esc(x.status)}</span></td>
      <td><span class="badge ${esc(x.codex_status || 'pending')}" title="${codexTitle}">${CODEX_STATUS[x.codex_status] || '待授权'}</span></td>
      <td><span class="badge ${esc(x.sub2api_status || 'not_imported')}" title="${sub2apiTitle}">${SUB2API_STATUS[x.sub2api_status] || '未导入'}</span></td>
      <td class="ship-cell">
        <span class="badge ${x.shipped ? 'registered' : 'pending'}" title="下载后自动标记，不能手动修改">${x.shipped ? '已出库' : '未出库'}</span>
      </td>
      <td>
        <button class="icon-btn" title="日志" onclick="showLog(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/><path d="M8 13h8M8 17h5"/></svg>
        </button>
        <button class="icon-btn" title="复制 AT" ${canCopyAT ? '' : 'disabled'} onclick="copyAccAT(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg>
        </button>
        <button class="icon-btn" title="复制邮箱----取件URL" onclick="copyMailboxLinks([${x.id}])">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/></svg>
        </button>
        <button class="icon-btn" title="检测 AT、套餐与0元试用资格" ${canCopyAT && atStatus !== 'checking' ? '' : 'disabled'} onclick="checkAT([${x.id}])">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M20 11a8 8 0 1 1-2.34-5.66"/><path d="M20 4v7h-7"/><path d="m9 12 2 2 4-4"/></svg>
        </button>
        <button class="icon-btn" title="检查 Plus 开通邮件" onclick="checkPlusMail([${x.id}])">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="5" width="18" height="14" rx="2"/><path d="m3 7 9 6 9-6"/><path d="m16 3 1 2 2 .5-1.5 1.5.5 2-2-1-2 1 .5-2L13 5.5l2-.5z"/></svg>
        </button>
        <button class="icon-btn" title="${x.codex_status === 'authorized' ? '重新获取 Codex OAuth' : '获取 Codex OAuth'}" ${canAuthorizeCodex ? '' : 'disabled'} onclick="authorizeCodex(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><circle cx="7.5" cy="15.5" r="5.5"/><path d="m21 2-9.6 9.6M15 8l3 3M18 5l3 3"/></svg>
        </button>
        <button class="icon-btn" title="${x.sub2api_status === 'imported' ? '更新 Sub2API' : '导入 Sub2API'}" ${canImportSub2API ? '' : 'disabled'} onclick="importSub2API(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/></svg>
        </button>
        <button class="icon-btn" title="下载" ${canDownload ? '' : 'disabled'} onclick="downloadAcc(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/><path d="M12 15V3"/></svg>
        </button>
        <button class="icon-btn danger" title="删除" onclick="del(${x.id})">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M10 11v6M14 11v6"/></svg>
        </button>
      </td>
    </tr>`;
}

async function loadAccountCategories() {
  const r = await api('/api/categories?scope=account');
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return;
  accountCategories = d.data || [];
  rebuildCategorySelect('filter-account-category', '全部分类', true);
  rebuildCategorySelect('batch-account-category', '批量归类', true);
  renderAccountCategories();
}

async function loadRegisterSources() {
  const [categoryResponse, mailboxResponse, proxyPoolResponse] = await Promise.all([
    api('/api/categories?scope=mailbox'),
    api('/api/mailboxes/options'),
    api('/api/proxy-pools'),
  ]);
  const categoryData = await categoryResponse.json().catch(() => ({}));
  const mailboxData = await mailboxResponse.json().catch(() => ({}));
  const proxyPoolData = await proxyPoolResponse.json().catch(() => ({}));
  if (categoryResponse.ok) {
    mailboxCategories = categoryData.data || [];
    rebuildRegisterCategorySelect();
  }
  if (proxyPoolResponse.ok) {
    proxyPools = proxyPoolData.data || [];
    defaultProxyPoolID = Number(proxyPoolData.default_proxy_pool_id) || 0;
    rebuildRegisterProxyPoolSelect();
  }
  if (!mailboxResponse.ok) return;
  registerMailboxes = mailboxData.data || [];
  const available = new Set(registerMailboxes.map(mailbox => mailbox.id));
  [...registerMailboxSelected].forEach(id => { if (!available.has(id)) registerMailboxSelected.delete(id); });
  renderRegisterMailboxes();
  updateRegisterMailboxLabel();
}

function rebuildCategorySelect(id, placeholder, includeUncategorized) {
  const select = document.getElementById(id);
  if (!select) return;
  const current = select.value;
  select.innerHTML = `<option value="">${esc(placeholder)}</option>` +
    (includeUncategorized ? '<option value="uncategorized">未分类</option>' : '') +
    accountCategories.map(category => `<option value="${category.id}">${esc(category.name)} (${category.item_count || 0})</option>`).join('');
  if ([...select.options].some(option => option.value === current)) select.value = current;
  if (select._rebuild) select._rebuild();
}

function openCategoryModal() {
  document.getElementById('new-account-category').value = '';
  document.getElementById('category-modal').style.display = 'flex';
  loadAccountCategories();
}

function renderAccountCategories() {
  const list = document.getElementById('account-category-list');
  if (!list) return;
  list.innerHTML = accountCategories.map(category => `
    <div class="category-item">
      <div><strong>${esc(category.name)}</strong><span>${Number(category.item_count || 0)} 个账户</span></div>
      <div>
        <button class="icon-btn" title="重命名" onclick="renameAccountCategory(${category.id})">改</button>
        <button class="icon-btn danger" title="删除" onclick="deleteAccountCategory(${category.id})">删</button>
      </div>
    </div>`).join('') || '<div class="category-empty">暂无分类</div>';
}

async function createAccountCategory() {
  const input = document.getElementById('new-account-category');
  const name = input.value.trim();
  if (!name) return toast('请输入分类名称', true);
  const r = await api('/api/categories', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ scope: 'account', name }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '新增分类失败', true);
  input.value = '';
  toast('分类已新增');
  loadAccountCategories();
}

async function renameAccountCategory(id) {
  const category = accountCategories.find(item => item.id === id);
  if (!category) return;
  const name = prompt('输入新的分类名称', category.name);
  if (name == null || !name.trim() || name.trim() === category.name) return;
  const r = await api('/api/categories/' + id, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name: name.trim() }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '重命名失败', true);
  toast('分类已重命名');
  loadAccountCategories();
  load();
}

async function deleteAccountCategory(id) {
  const category = accountCategories.find(item => item.id === id);
  if (!category || !confirm(`确定删除分类“${category.name}”？其中账户将变为未分类。`)) return;
  const r = await api('/api/categories/' + id, { method: 'DELETE' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '删除分类失败', true);
  toast('分类已删除');
  loadAccountCategories();
  load();
}

async function assignSelectedCategory(value) {
  if (!value || !accSelected.size) return;
  const categoryId = value === 'uncategorized' ? null : Number(value);
  const r = await api('/api/categories/assign', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ scope: 'account', ids: [...accSelected], category_id: categoryId }),
  });
  const d = await r.json().catch(() => ({}));
  document.getElementById('batch-account-category').value = '';
  syncSelect('batch-account-category');
  if (!r.ok) return toast(d.error || '批量归类失败', true);
  toast('已更新 ' + accSelected.size + ' 个账户分类');
  loadAccountCategories();
  load();
}

async function checkAT(ids) {
  if (!ids.length) return;
  const r = await api('/api/registrations/at-check', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '启动 AT 与试用资格检测失败', true);
  toast('已开始检测 ' + Number(d.scheduled || 0) + ' 个 AT 与试用资格');
  load();
}

function checkSelectedAT() {
  return checkAT([...accSelected]);
}

async function checkPlusMail(ids) {
  if (!ids.length || plusMailChecking) return;
  plusMailChecking = true;
  const batchButton = document.getElementById('batch-plus-mail-btn');
  const progress = document.getElementById('plus-mail-progress');
  if (batchButton) {
    batchButton.disabled = true;
    batchButton.textContent = '检查中...';
  }
  if (progress) {
    progress.textContent = '正在检查 ' + ids.length + ' 个邮箱，请稍候...';
    progress.style.display = 'inline';
  }
  toast('已开始检查 ' + ids.length + ' 个 Plus 邮箱');
  try {
    const r = await api('/api/registrations/plus-mail-check', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ ids }),
    });
    const d = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(d.error || 'Plus 邮件检查失败');
    const found = Number(d.found || 0);
    const notFound = Number(d.not_found || 0);
    const failed = Number(d.failed || 0);
    const result = '检查完成：确认 ' + found + '，未发现 ' + notFound + (failed ? '，失败 ' + failed : '');
    if (progress) progress.textContent = result;
    toast('Plus 邮件' + result, failed > 0 && found === 0);
    await load();
  } catch (error) {
    const message = error && error.message ? error.message : 'Plus 邮件检查失败';
    if (progress) progress.textContent = '检查失败：' + message;
    toast(message, true);
  } finally {
    plusMailChecking = false;
    if (batchButton) {
      batchButton.disabled = false;
      batchButton.textContent = '检查 Plus 邮件';
    }
    syncBatchBar();
  }
}

function checkSelectedPlusMail() {
  return checkPlusMail([...accSelected]);
}

function rebuildRegisterProxyPoolSelect() {
  const select = document.getElementById('register-proxy-pool');
  if (!select) return;
  const current = select.dataset.loaded === '1' ? Number(select.value) : defaultProxyPoolID;
  select.innerHTML = '<option value="0">直连</option>' + proxyPools.filter(pool => Number(pool.proxy_count) > 0).map(pool =>
    `<option value="${pool.id}">${esc(pool.name)} (${Number(pool.proxy_count)})</option>`
  ).join('');
  select.value = [...select.options].some(option => Number(option.value) === current) ? String(current) : '0';
  select.dataset.loaded = '1';
  if (select._rebuild) select._rebuild();
}

function rebuildRegisterCategorySelect() {
  const select = document.getElementById('register-category');
  const current = select.value;
  select.innerHTML = '<option value="">选择邮箱分组</option>' + mailboxCategories.map(category =>
    `<option value="${category.id}">${esc(category.name)} (${category.item_count || 0})</option>`
  ).join('');
  if ([...select.options].some(option => option.value === current)) select.value = current;
  if (select._rebuild) select._rebuild();
}

function updateRegisterScope() {
  const scope = document.getElementById('register-scope').value;
  const category = document.getElementById('register-category');
  const picker = document.getElementById('register-mailbox-picker');
  category.parentElement.style.display = scope === 'category' ? '' : 'none';
  picker.style.display = scope === 'mailboxes' ? '' : 'none';
  if (scope !== 'mailboxes') closeMailboxPicker();
}

function toggleMailboxPicker(event) {
  event.stopPropagation();
  document.getElementById('register-mailbox-panel').classList.toggle('open');
  if (document.getElementById('register-mailbox-panel').classList.contains('open')) {
    document.getElementById('register-mailbox-search').focus();
  }
}

function closeMailboxPicker() {
  document.getElementById('register-mailbox-panel').classList.remove('open');
}

function filteredRegisterMailboxes() {
  const query = document.getElementById('register-mailbox-search').value.trim().toLowerCase();
  return query ? registerMailboxes.filter(mailbox => mailbox.email.toLowerCase().includes(query)) : registerMailboxes;
}

function renderRegisterMailboxes() {
  const mailboxes = filteredRegisterMailboxes();
  document.getElementById('register-mailbox-options').innerHTML = mailboxes.map(mailbox => `
    <label class="mailbox-picker-option">
      <input type="checkbox" value="${mailbox.id}" ${registerMailboxSelected.has(mailbox.id) ? 'checked' : ''} onchange="toggleRegisterMailbox(${mailbox.id}, this.checked)">
      <span>${esc(mailbox.email)}</span>
    </label>`).join('') || '<div class="mailbox-picker-empty">没有匹配的已验证邮箱</div>';
}

function toggleRegisterMailbox(id, checked) {
  if (checked) registerMailboxSelected.add(id); else registerMailboxSelected.delete(id);
  updateRegisterMailboxLabel();
}

function selectVisibleRegisterMailboxes() {
  filteredRegisterMailboxes().forEach(mailbox => registerMailboxSelected.add(mailbox.id));
  renderRegisterMailboxes();
  updateRegisterMailboxLabel();
}

function clearRegisterMailboxes() {
  registerMailboxSelected.clear();
  renderRegisterMailboxes();
  updateRegisterMailboxLabel();
}

function updateRegisterMailboxLabel() {
  const button = document.getElementById('register-mailbox-btn');
  button.textContent = registerMailboxSelected.size ? `已选 ${registerMailboxSelected.size} 个邮箱` : '选择邮箱';
}

document.addEventListener('click', closeMailboxPicker);

/* ===== 注册进度 ===== */
async function loadProduce() {
  try {
    const r = await api('/api/produce/status');
    const s = await r.json();
    document.getElementById('pd-pending').textContent = s.pending || 0;
    document.getElementById('pd-running').textContent = s.running_num || 0;
    document.getElementById('pd-registered').textContent = s.registered || 0;
    document.getElementById('pd-failed').textContent = s.failed || 0;
    document.getElementById('pd-stop').style.display = s.running ? '' : 'none';
  } catch (e) { /* ignore */ }
}

/* 浏览器就绪状态：未就绪禁用注册 */
let browserReady = true;
async function loadBrowserGate() {
  try {
    const s = await (await api('/api/browser/status')).json();
    browserReady = !!s.ready;
    const btn = document.getElementById('produce-btn');
    if (btn) {
      btn.disabled = !browserReady;
      btn.title = browserReady ? '' : (s.message || '缺少浏览器');
    }
    const msg = document.getElementById('pd-msg');
    if (!browserReady && msg) msg.textContent = '⚠ ' + (s.message || '缺少浏览器，暂不能注册');
  } catch (e) { /* ignore */ }
}

async function startProduce() {
  if (!browserReady) return toast('缺少浏览器，正在下载或下载失败，暂不能注册', true);
  const count = parseInt(document.getElementById('register-count').value, 10);
  if (!count || count < 1) return toast('请输入有效注册数量', true);
  const scope = document.getElementById('register-scope').value;
  const proxyPoolSelect = document.getElementById('register-proxy-pool');
  const proxyPoolID = Number(proxyPoolSelect.value) || 0;
  const body = { count, scope, proxy_pool_id: proxyPoolID };
  if (scope === 'category') {
    body.category_id = Number(document.getElementById('register-category').value);
    if (!body.category_id) return toast('请选择邮箱分组', true);
  }
  if (scope === 'mailboxes') {
    body.mailbox_ids = [...registerMailboxSelected];
    if (!body.mailbox_ids.length) return toast('请选择至少一个邮箱', true);
  }
  const r = await api('/api/produce', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    return toast(d.error || '启动注册失败', true);
  }
  toast('已开始注册 ' + count + ' 个账号 · ' + proxyPoolSelect.options[proxyPoolSelect.selectedIndex].text);
  loadProduce();
  load();
}

async function stopProduce() {
  if (!confirm('确定停止当前注册任务?')) return;
  await api('/api/produce/stop', { method: 'POST' });
  toast('已请求停止');
  loadProduce();
}

let logTimer = null;
let logAccId = null;

async function showLog(id) {
  logAccId = id;
  // 清空旧日志，避免打开新账号时残留上一次的内容
  document.getElementById('log-title').textContent = '执行日志';
  document.getElementById('log-body').textContent = '加载中...';
  document.getElementById('log-shot-btn').style.display = 'none';
  document.getElementById('log-modal').style.display = 'flex';
  document.body.style.overflow = 'hidden';
  await refreshLog(false);
  clearInterval(logTimer);
  logTimer = setInterval(() => refreshLog(true), 2000);
}

async function refreshLog(silent) {
  if (logAccId == null) return;
  const r = await api('/api/registrations/' + logAccId + '/logs');
  if (!r.ok) { if (!silent) toast('读取日志失败', true); return; }
  const d = await r.json();
  document.getElementById('log-title').textContent = '执行日志 · ' + d.email;
  document.getElementById('log-shot-btn').style.display = d.has_shot ? '' : 'none';
  const parts = [];
  if (d.note) parts.push('备注: ' + d.note);
  if (parts.length) parts.push('');
  parts.push(d.log || '（无执行日志）');
  document.getElementById('log-body').textContent = parts.join('\n');
}

function closeLog() {
  clearInterval(logTimer);
  logTimer = null;
  logAccId = null;
  document.getElementById('log-modal').style.display = 'none';
  document.body.style.overflow = '';
  document.getElementById('log-body').textContent = '';
}

/* ===== 异常截图 ===== */
async function viewShot() {
  if (logAccId == null) return;
  const r = await api('/api/registrations/' + logAccId + '/shot');
  if (!r.ok) return toast('暂无异常截图', true);
  const blob = await r.blob();
  const img = document.getElementById('shot-img');
  if (img.dataset.url) URL.revokeObjectURL(img.dataset.url);
  img.src = img.dataset.url = URL.createObjectURL(blob);
  document.getElementById('shot-modal').style.display = 'flex';
}
function closeShot() {
  document.getElementById('shot-modal').style.display = 'none';
}

/* ===== 多选 ===== */
function toggleSelect(id, checked) {
  if (checked) accSelected.add(id); else accSelected.delete(id);
  syncBatchBar();
}
function toggleSelectAll(checked) {
  Object.keys(accCache).forEach(id => {
    if (checked) accSelected.add(Number(id)); else accSelected.delete(Number(id));
  });
  load();
}
function clearSelection() { accSelected.clear(); load(); }
function syncBatchBar() {
  const bar = document.getElementById('acc-batch');
  bar.style.display = accSelected.size || plusMailChecking ? 'flex' : 'none';
  document.getElementById('acc-batch-count').textContent = '已选 ' + accSelected.size + ' 项';
  const all = document.getElementById('acc-check-all');
  const ids = Object.keys(accCache).map(Number);
  all.checked = ids.length > 0 && ids.every(id => accSelected.has(id));
}

/* ===== 复制 AT（按所选顺序，一行一个） ===== */
async function copySelectedAT() {
  const ids = [...accSelected];
  if (!ids.length) return;
  const r = await api('/api/access-tokens', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '读取 AT 失败', true);
  const tokens = Array.isArray(d.tokens) ? d.tokens.filter(Boolean) : [];
  if (!tokens.length) return toast('所选账号没有可复制的 AT', true);
  try {
    await copyText(tokens.join('\n'));
  } catch (e) {
    return toast('复制失败，请检查浏览器剪贴板权限', true);
  }
  const skipped = Number(d.skipped) || 0;
  toast('已复制 ' + tokens.length + ' 个 AT' + (skipped ? '，跳过 ' + skipped + ' 项' : ''));
}

async function copyAccAT(id) {
  const r = await api('/api/access-tokens', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids: [id] }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '读取 AT 失败', true);
  const tokens = Array.isArray(d.tokens) ? d.tokens.filter(Boolean) : [];
  if (!tokens.length) return toast('该账号没有可复制的 AT', true);
  try {
    await copyText(tokens[0]);
  } catch (e) {
    return toast('复制失败，请检查浏览器剪贴板权限', true);
  }
  toast('AT 已复制');
}

function copySelectedMailboxLinks() {
  return copyMailboxLinks([...accSelected]);
}

async function copyMailboxLinks(ids) {
  if (!ids.length) return;
  const r = await api('/api/registrations/mailbox-links', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || '读取邮箱取件 URL 失败', true);
  const items = Array.isArray(d.items) ? d.items : [];
  if (!items.length) return toast('所选账号没有配置取件 URL', true);
  const text = items.map(item => item.email + '----' + item.code_url).join('\n');
  try {
    await copyText(text);
  } catch (e) {
    return toast('复制失败，请检查浏览器剪贴板权限', true);
  }
  const skipped = Number(d.skipped) || 0;
  toast('已复制 ' + items.length + ' 条邮箱取件信息' + (skipped ? '，跳过 ' + skipped + ' 项' : ''));
}

async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return;
  }
  const textarea = document.createElement('textarea');
  textarea.value = text;
  textarea.style.position = 'fixed';
  textarea.style.opacity = '0';
  document.body.appendChild(textarea);
  textarea.select();
  const copied = document.execCommand('copy');
  textarea.remove();
  if (!copied) throw new Error('copy failed');
}

/* ===== Codex OAuth / Sub2API ===== */
async function authorizeCodex(id) {
  if (!confirm('开始获取该账号的 Codex OAuth？授权过程中可能按需购买接码号码。')) return;
  const r = await api('/api/registrations/' + id + '/codex-authorize', { method: 'POST' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || 'Codex 授权失败', true);
  toast('Codex OAuth 已获取');
  load();
}

async function importSub2API(id) {
  const r = await api('/api/registrations/' + id + '/sub2api-import', { method: 'POST' });
  const d = await r.json().catch(() => ({}));
  if (!r.ok) return toast(d.error || 'Sub2API 导入失败', true);
  toast('Sub2API ' + (d.action === 'updated' ? '更新' : '导入') + '成功 · 远端账号 #' + d.account_id);
  load();
}

/* ===== 下载（auth JSON，单个→对象，多个→数组；下载即出库） ===== */
async function downloadAcc(id) {
  await downloadByIds([id], 'auth_' + id + '.json');
}
async function downloadSelected() {
  const ids = [...accSelected];
  if (!ids.length) return;
  await downloadByIds(ids, 'auth_' + ids.length + '.json');
}
async function downloadByIds(ids, filename) {
  const r = await api('/api/download', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ids }),
  });
  if (!r.ok) {
    const d = await r.json().catch(() => ({}));
    return toast(d.error || '下载失败', true);
  }
  const blob = await r.blob();
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = filename;
  a.click();
  URL.revokeObjectURL(a.href);
  load(); // 刷新出库状态
}

/* ===== 删除 ===== */
async function delSelected() {
  const ids = [...accSelected];
  if (!ids.length) return;
  if (!confirm('确定删除所选 ' + ids.length + ' 个账户?')) return;
  for (const id of ids) {
    await api('/api/registrations/' + id, { method: 'DELETE' });
    accSelected.delete(id);
  }
  toast('已删除 ' + ids.length + ' 个');
  load();
}
async function del(id) {
  if (!confirm('确定删除账户 #' + id + ' ?')) return;
  const r = await api('/api/registrations/' + id, { method: 'DELETE' });
  if (!r.ok) return toast('删除失败', true);
  accSelected.delete(id);
  toast('已删除');
  load();
}

document.getElementById('search').addEventListener('keydown', e => {
  if (e.key === 'Enter') { page = 1; load(); }
});
['filter-status', 'filter-at-status', 'filter-plan', 'filter-trial-status', 'filter-register-country', 'filter-account-category'].forEach(id => {
  document.getElementById(id).addEventListener('change', () => { page = 1; load(); });
});
document.getElementById('new-account-category').addEventListener('keydown', event => {
  if (event.key === 'Enter') createAccountCategory();
});

loadAccountCategories();
loadRegisterSources();
updateRegisterScope();
load();
loadProduce();
loadBrowserGate();
setInterval(load, 3000);
setInterval(loadProduce, 2000);
setInterval(loadBrowserGate, 2500);
