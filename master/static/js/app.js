// ═══════════════════════════════════════════════════════════════
// State
// ═══════════════════════════════════════════════════════════════
const state = {
  mode:       'terminal',       // 'terminal' | 'command'
  selectedNodeId: null,
  cpu:        0,
  memory:     0,
  memUnit:    'GB',
  device_num: 0,
  cmdUserId:  localStorage.getItem('neu_box_cmd_user') || '',
};

const limits = {
  cpu:        { min: 0, max: 64   },
  memory:     { min: 0, max: 256  },
  device_num: { min: 0, max: 16   },
};

// ═══════════════════════════════════════════════════════════════
// DOM refs
// ═══════════════════════════════════════════════════════════════
const form              = document.getElementById('mainForm');
const submitBtn         = document.getElementById('submitBtn');
const resultDiv         = document.getElementById('result');
const toast             = document.getElementById('toast');
const memUnitEl         = document.getElementById('memUnit');
const nodeList          = document.getElementById('nodeList');
const nodeCount         = document.getElementById('nodeCount');
const refreshBtn        = document.getElementById('refreshBtn');

// Mode toggle
const modeToggle        = document.getElementById('modeToggle');
const terminalFields    = document.getElementById('terminalFields');
const commandFields     = document.getElementById('commandFields');

// Terminal fields
const usernameInput     = document.getElementById('usernameInput');
const passwordInput     = document.getElementById('passwordInput');

// Command fields
const cmdUserIdEl       = document.getElementById('cmdUserId');
const cmdPasswordEl     = document.getElementById('cmdPassword');
const cmdInputEl        = document.getElementById('cmdInput');

// Queue
const queueList         = document.getElementById('queueList');
const queueRefreshBtn   = document.getElementById('queueRefreshBtn');

// Right panel
const rightPanel        = document.getElementById('rightPanel');
const terminalHeader    = document.getElementById('terminalHeader');
const terminalPlaceholder = document.getElementById('terminalPlaceholder');
const terminalIframe    = document.getElementById('terminalIframe');
const terminalUrlEl     = document.getElementById('terminalUrl');
const terminalClose     = document.getElementById('terminalClose');
const logViewer         = document.getElementById('logViewer');
const logPlaceholder    = document.getElementById('logPlaceholder');
const logContent        = document.getElementById('logContent');

// Init command user ID
if (state.cmdUserId) cmdUserIdEl.value = state.cmdUserId;

// ═══════════════════════════════════════════════════════════════
// Formatting helpers
// ═══════════════════════════════════════════════════════════════

function formatBytes(bytes) {
  if (!bytes || bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, val = bytes;
  while (val >= 1024 && i < units.length - 1) { val /= 1024; i++; }
  return val >= 100 ? `${Math.round(val)} ${units[i]}` : `${val.toFixed(1)} ${units[i]}`;
}

function isIdlePercent(cpuIdle) {
  return typeof cpuIdle === 'number' && cpuIdle !== Math.floor(cpuIdle);
}

function formatCpu(cpuIdle, cpuTotal) {
  if (!cpuTotal) return '? / ? 核';
  if (isIdlePercent(cpuIdle)) {
    const usedCores = ((100 - cpuIdle) / 100) * cpuTotal;
    return `${usedCores.toFixed(1)} / ${cpuTotal} 核`;
  }
  return `${cpuTotal - cpuIdle} / ${cpuTotal} 核`;
}

function cpuUsedPercent(cpuIdle, cpuTotal) {
  if (!cpuTotal) return 0;
  if (isIdlePercent(cpuIdle)) return 100 - cpuIdle;
  return ((cpuTotal - cpuIdle) / cpuTotal) * 100;
}

function escapeHtml(str) {
  const div = document.createElement('div');
  div.textContent = str;
  return div.innerHTML;
}

function formatTime(ts) {
  if (!ts) return '—';
  const d = new Date(ts * 1000);
  const pad = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth()+1)}-${pad(d.getDate())} ` +
         `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

// ═══════════════════════════════════════════════════════════════
// Toast
// ═══════════════════════════════════════════════════════════════

function showToast(msg, type) {
  toast.textContent = msg;
  toast.className = `toast ${type}`;
  toast.style.display = 'block';
  setTimeout(() => { toast.style.display = 'none'; }, 3000);
}

// ═══════════════════════════════════════════════════════════════
// Mode switching
// ═══════════════════════════════════════════════════════════════

function switchMode(mode) {
  if (mode === state.mode) return;
  state.mode = mode;

  modeToggle.querySelectorAll('button').forEach(b => {
    b.classList.toggle('active', b.dataset.mode === mode);
  });

  if (mode === 'terminal') {
    terminalFields.style.display = '';
    commandFields.style.display = 'none';
    submitBtn.textContent = '申请终端';
    rightPanel.classList.add('mode-terminal');
    rightPanel.classList.remove('mode-command');
  } else {
    terminalFields.style.display = 'none';
    commandFields.style.display = '';
    submitBtn.textContent = '提交命令';
    rightPanel.classList.add('mode-command');
    rightPanel.classList.remove('mode-terminal');
    // Reset log viewer to placeholder
    logPlaceholder.style.display = '';
    logContent.style.display = 'none';
    logContent.innerHTML = '';
  }

  updateSubmitBtn();
  resultDiv.style.display = 'none';
}

modeToggle.addEventListener('click', (e) => {
  const btn = e.target.closest('button');
  if (!btn) return;
  switchMode(btn.dataset.mode);
});

// Init mode classes
rightPanel.classList.add('mode-terminal');

// ═══════════════════════════════════════════════════════════════
// Node rendering
// ═══════════════════════════════════════════════════════════════

function renderDeviceChips(idle) {
  if (idle === 0) return '<span class="device-text">无</span>';
  let html = '<span class="device-chips">';
  for (let i = 0; i < idle; i++) {
    html += `<span class="device-chip idle"></span>`;
  }
  html += '</span>';
  html += `<span class="device-text">${idle} 可用</span>`;
  return html;
}

function progressClass(percent) {
  return percent > 85 ? 'high' : '';
}

function renderNodeCard(node) {
  const isSelected = node.node_id === state.selectedNodeId;
  const isOnline  = node.status === 'online';
  const cpuPct     = cpuUsedPercent(node.idle_cpu, node.total_cpu);
  const memUsed    = node.total_mem - node.idle_mem;
  const memPct     = node.total_mem > 0 ? (memUsed / node.total_mem) * 100 : 0;
  const memClass   = node.total_mem > 0 ? progressClass(memPct) : '';
  const cpuClass   = node.total_cpu > 0 ? progressClass(cpuPct) : '';

  return `
    <div class="node-card ${isSelected ? 'selected' : ''}"
         data-node-id="${node.node_id}"
         role="button" tabindex="0">
      <div class="node-card-header">
        <span class="node-card-addr">${node.name}</span>
        <span class="node-status-dot ${isOnline ? 'online' : 'offline'}"
              title="${isOnline ? '在线' : '离线'}"></span>
      </div>
      ${isOnline ? `
      <div class="node-resources">
        <div class="resource-row">
          <span class="resource-label">CPU</span>
          <div class="progress-bar">
            <div class="progress-fill ${cpuClass}" style="width:${cpuPct}%"></div>
          </div>
          <span class="resource-text">${formatCpu(node.idle_cpu, node.total_cpu)}</span>
        </div>
        <div class="resource-row">
          <span class="resource-label">MEM</span>
          <div class="progress-bar">
            <div class="progress-fill mem ${memClass}" style="width:${memPct}%"></div>
          </div>
          <span class="resource-text">${formatBytes(memUsed)} / ${formatBytes(node.total_mem)}</span>
        </div>
        <div class="device-row">
          <span class="device-label">GPU</span>
          ${renderDeviceChips(node.idle_gpu)}
        </div>
        <div class="device-row">
          <span class="device-label">NPU</span>
          ${renderDeviceChips(node.idle_npu)}
        </div>
        ${node.active_sandboxes > 0 ? `
        <div class="sandbox-count">${node.active_sandboxes} 个沙盒运行中</div>
        ` : ''}
      </div>
      ` : `
      <div class="node-resources">
        <span style="font-size:10px;color:var(--sub)">节点离线</span>
      </div>
      `}
    </div>`;
}

function renderNodeCards(nodes) {
  nodeList.innerHTML = nodes.map(renderNodeCard).join('');

  nodeList.querySelectorAll('.node-card').forEach(card => {
    card.addEventListener('click', () => {
      const nodeId = card.dataset.nodeId;
      if (!nodeId) return;
      selectNode(nodeId, nodes);
    });
  });
}

function selectNode(nodeId, nodes) {
  state.selectedNodeId = nodeId;
  renderNodeCards(nodes);
  updateSubmitBtn();
  // Refresh queue when switching nodes
  fetchQueue();
}

// ═══════════════════════════════════════════════════════════════
// Node fetching
// ═══════════════════════════════════════════════════════════════

async function fetchNodes() {
  try {
    const resp = await fetch('/nodes/get_all_nodes', { method: 'POST' });
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();

    const nodes = data.nodes || [];

    if (nodes.length === 0) {
      nodeList.innerHTML = `
        <div class="node-card node-card-loading">
          <span class="node-status-dot offline"></span>
          <span style="color:var(--sub)">无可用节点</span>
        </div>`;
      nodeCount.textContent = '无可用';
      submitBtn.disabled = true;
      return;
    }

    nodeCount.textContent = `${nodes.length} 个可用`;

    const currentSelected = nodes.find(n => n.node_id === state.selectedNodeId);
    if (!currentSelected) {
      const firstOnline = nodes.find(n => n.status === 'online');
      if (firstOnline) {
        state.selectedNodeId = firstOnline.node_id;
      }
    }

    renderNodeCards(nodes);
    updateSubmitBtn();

  } catch (err) {
    nodeList.innerHTML = `
      <div class="node-card node-card-loading">
        <span class="node-status-dot offline"></span>
        <span style="color:var(--sub)">节点加载失败</span>
      </div>`;
    nodeCount.textContent = '加载失败';
    showToast('节点列表加载失败: ' + err.message, 'error');
    submitBtn.disabled = true;
  }
}

fetchNodes();
setInterval(fetchNodes, 60000);

// ═══════════════════════════════════════════════════════════════
// Submit button logic
// ═══════════════════════════════════════════════════════════════

function isSelectedNodeOnline() {
  const cards = nodeList.querySelectorAll('.node-card');
  for (const card of cards) {
    if (card.dataset.nodeId === state.selectedNodeId) {
      const dot = card.querySelector('.node-status-dot');
      return dot && dot.classList.contains('online');
    }
  }
  return false;
}

function updateSubmitBtn() {
  const hasNode = !!state.selectedNodeId;
  const online = hasNode && isSelectedNodeOnline();

  if (!hasNode || !online) {
    submitBtn.disabled = true;
    return;
  }

  if (state.mode === 'terminal') {
    submitBtn.disabled = !(usernameInput.value.trim() && passwordInput.value);
  } else {
    submitBtn.disabled = !(cmdUserIdEl.value.trim() && cmdPasswordEl.value && cmdInputEl.value.trim());
  }
}

// Listen to input changes
usernameInput.addEventListener('input', updateSubmitBtn);
passwordInput.addEventListener('input', updateSubmitBtn);
cmdUserIdEl.addEventListener('input', () => {
  state.cmdUserId = cmdUserIdEl.value.trim();
  localStorage.setItem('neu_box_cmd_user', state.cmdUserId);
  updateSubmitBtn();
});
cmdPasswordEl.addEventListener('input', updateSubmitBtn);
cmdInputEl.addEventListener('input', updateSubmitBtn);

// ═══════════════════════════════════════════════════════════════
// Stepper events
// ═══════════════════════════════════════════════════════════════

function setValDisplay(el, field, val) {
  if (val === 0) {
    if (el.tagName === 'INPUT') el.value = '不限制';
    else el.textContent = '不限制';
    el.classList.add('no-limit');
  } else {
    if (el.tagName === 'INPUT') el.value = val;
    else el.textContent = val;
    el.classList.remove('no-limit');
  }
}

function parseValInput(el, field) {
  const raw = (el.tagName === 'INPUT' ? el.value : el.textContent).trim();
  if (raw === '不限制' || raw === '') return 0;
  const n = parseInt(raw, 10);
  if (isNaN(n) || n < 0) return state[field];
  return n;
}

document.querySelectorAll('.stepper').forEach(stepper => {
  const field = stepper.dataset.field;
  const valEl = stepper.querySelector('.value');

  stepper.addEventListener('click', (e) => {
    const btn = e.target.closest('button');
    if (!btn) return;

    const action = btn.dataset.action;
    const lim    = limits[field];
    const cur    = state[field];
    const next   = action === 'up' ? cur + 1 : cur - 1;

    if (next < lim.min || next > lim.max) return;

    state[field] = next;
    setValDisplay(valEl, field, next);
    updateButtons(stepper, field);
  });

  const commitInput = () => {
    let val = parseValInput(valEl, field);
    const lim = limits[field];
    if (val < lim.min) val = lim.min;
    if (val > lim.max) val = lim.max;
    state[field] = val;
    setValDisplay(valEl, field, val);
    updateButtons(stepper, field);
  };

  valEl.addEventListener('blur', commitInput);
  valEl.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      valEl.blur();
    }
  });

  setValDisplay(valEl, field, state[field]);
  updateButtons(stepper, field);
});

function updateButtons(stepper, field) {
  const lim = limits[field];
  const cur = state[field];
  stepper.querySelector('[data-action=down]').disabled = cur <= lim.min;
  stepper.querySelector('[data-action=up]').disabled   = cur >= lim.max;
}

// ═══════════════════════════════════════════════════════════════
// Memory unit toggle
// ═══════════════════════════════════════════════════════════════

memUnitEl.addEventListener('click', (e) => {
  const btn = e.target.closest('button');
  if (!btn) return;

  memUnitEl.querySelectorAll('button').forEach(b => b.classList.remove('active'));
  btn.classList.add('active');
  state.memUnit = btn.dataset.unit;

  if (state.memUnit === 'GB') {
    limits.memory = { min: 0, max: 256 };
    if (state.memory > 256) state.memory = 256;
  } else {
    limits.memory = { min: 0, max: 65536 };
    if (state.memory > 65536) state.memory = 65536;
  }

  const memStepper = document.querySelector('[data-field=memory]');
  const memValEl = memStepper.querySelector('.value');
  setValDisplay(memValEl, 'memory', state.memory);
  updateButtons(memStepper, 'memory');
});

// ═══════════════════════════════════════════════════════════════
// Terminal
// ═══════════════════════════════════════════════════════════════

function openTerminal(url) {
  terminalIframe.src = url;
  terminalUrlEl.textContent = url;
  rightPanel.classList.add('active');
}

function closeTerminal() {
  terminalIframe.src = '';
  terminalUrlEl.textContent = '';
  rightPanel.classList.remove('active');
}

terminalClose.addEventListener('click', closeTerminal);

// ═══════════════════════════════════════════════════════════════
// Queue
// ═══════════════════════════════════════════════════════════════

function renderQueue(data) {
  const queue = data.queue || [];
  if (queue.length === 0) {
    queueList.innerHTML = '<div class="queue-empty">队列为空</div>';
    return;
  }

  queueList.innerHTML = queue.map(task => {
    const isRunning = task.status === 'running';
    const posText = isRunning ? '▶' : (task.position || '?');
    const isOwn = state.cmdUserId && task.user_id === state.cmdUserId;
    const isDone = task.status === 'completed' || task.status === 'failed';
    const clickable = isOwn && isDone;

    return `
      <div class="queue-item ${isRunning ? 'running' : ''} ${clickable ? 'clickable' : ''}"
           data-task-id="${task.task_id}"
           data-user-id="${escapeHtml(task.user_id)}"
           title="${clickable ? '点击查看日志' : ''}">
        <span class="queue-pos">${posText}</span>
        <span class="queue-user" title="${escapeHtml(task.user_id)}">${escapeHtml(task.user_id)}</span>
        <span class="queue-cmd" title="${escapeHtml(task.command)}">${escapeHtml(task.command)}</span>
        <span class="queue-status ${task.status}">${statusLabel(task.status)}</span>
      </div>`;
  }).join('');

  // Bind click: only own + completed/failed → view log
  queueList.querySelectorAll('.queue-item.clickable').forEach(item => {
    item.addEventListener('click', () => {
      const taskId = item.dataset.taskId;
      if (taskId) viewTaskLog(taskId);
    });
  });
}

function statusLabel(s) {
  const map = { queued: '排队中', running: '执行中', completed: '已完成', failed: '失败' };
  return map[s] || s;
}

async function fetchQueue() {
  if (!state.selectedNodeId) {
    queueList.innerHTML = '<div class="queue-empty">选择节点后刷新</div>';
    return;
  }

  try {
    const resp = await fetch(`/command/queue?node_id=${encodeURIComponent(state.selectedNodeId)}`);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();
    renderQueue(data);
  } catch (err) {
    queueList.innerHTML = `<div class="queue-empty">加载失败: ${err.message}</div>`;
  }
}

// Manual refresh only
queueRefreshBtn.addEventListener('click', () => {
  queueRefreshBtn.classList.add('spinning');
  fetchQueue().finally(() => queueRefreshBtn.classList.remove('spinning'));
});

// Also refresh when switching nodes (handled in selectNode)

// ═══════════════════════════════════════════════════════════════
// Log viewer
// ═══════════════════════════════════════════════════════════════

async function viewTaskLog(taskId) {
  if (!state.selectedNodeId) return;

  // 自动切换到命令模式
  if (state.mode !== 'command') {
    switchMode('command');
  }

  // 未填写用户标识
  if (!state.cmdUserId) {
    logPlaceholder.style.display = 'none';
    logContent.style.display = '';
    logContent.innerHTML = '<div style="color:#ff5f57;font-size:16px;text-align:center;padding:40px">🔒 需要用户标识<br><span style="font-size:13px;color:#8e8e93">请在左侧填写"用户标识"后重试</span></div>';
    return;
  }

  const password = cmdPasswordEl.value;
  if (!password) {
    logPlaceholder.style.display = 'none';
    logContent.style.display = '';
    logContent.innerHTML = '<div style="color:#ff5f57">请先输入密码</div>';
    return;
  }

  logPlaceholder.style.display = 'none';
  logContent.style.display = '';
  logContent.innerHTML = '<div style="color:#636366">加载中…</div>';

  try {
    const params = new URLSearchParams({
      node_id: state.selectedNodeId,
      user_id: state.cmdUserId,
      password: password,
    });
    const resp = await fetch(`/command/result/${taskId}?${params}`);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const task = await resp.json();

    // 密码错误 → 无权限
    if (task.permission_denied) {
      logContent.innerHTML = '<div style="color:#ff5f57;font-size:16px;text-align:center;padding:40px">🔒 无权限<br><span style="font-size:13px;color:#8e8e93">密码不正确</span></div>';
      return;
    }

    if (task.error) {
      logContent.innerHTML = `<div style="color:#ff5f57">错误: ${escapeHtml(task.error)}</div>`;
      return;
    }

    let html = `<div class="log-meta">`;
    html += `<strong>任务ID:</strong> ${escapeHtml(task.task_id)}<br>`;
    html += `<strong>用户:</strong> ${escapeHtml(task.user_id)}<br>`;
    html += `<strong>命令:</strong> ${escapeHtml(task.command)}<br>`;
    html += `<strong>资源:</strong> CPU=${task.cpu || 0}, 内存=${task.mem || '0'}, 设备=${task.device_num || 0}`;
    if (task.devices && task.devices.length > 0) {
      html += ` (${escapeHtml(task.devices.join(', '))})`;
    }
    html += `<br>`;
    html += `<strong>创建时间:</strong> ${formatTime(task.created_at)}<br>`;
    html += `<strong>状态:</strong> ${statusLabel(task.status)}`;
    if (task.result) {
      html += ` | <strong>返回码:</strong> ${task.result.returncode}`;
      if (task.result.timed_out) html += ` <span style="color:#ff5f57">(超时)</span>`;
    }
    html += `</div>`;

    if (task.result) {
      const r = task.result;
      if (r.stdout) {
        html += `<div class="log-stdout">${escapeHtml(r.stdout)}</div>`;
      }
      if (r.stderr) {
        html += `<div class="log-stderr">${escapeHtml(r.stderr)}</div>`;
      }
      if (!r.stdout && !r.stderr) {
        html += `<div class="log-no-output">(无输出)</div>`;
      }
    }

    logContent.innerHTML = html;

  } catch (err) {
    logContent.innerHTML = `<div style="color:#ff5f57">加载失败: ${err.message}</div>`;
  }
}

// ═══════════════════════════════════════════════════════════════
// Manual refresh
// ═══════════════════════════════════════════════════════════════

refreshBtn.addEventListener('click', () => {
  refreshBtn.classList.add('spinning');
  fetchNodes().finally(() => refreshBtn.classList.remove('spinning'));
});

// ═══════════════════════════════════════════════════════════════
// Form submit
// ═══════════════════════════════════════════════════════════════

form.addEventListener('submit', async (e) => {
  e.preventDefault();

  if (!state.selectedNodeId) {
    showToast('请先选择一个节点', 'error');
    return;
  }

  if (state.mode === 'terminal') {
    await submitTerminal();
  } else {
    await submitCommand();
  }
});

async function submitTerminal() {
  const username = usernameInput.value.trim();
  const password = passwordInput.value;
  if (!username) { showToast('请输入用户名', 'error'); return; }
  if (!password) { showToast('请输入密码', 'error'); return; }

  submitBtn.disabled = true;
  submitBtn.textContent = '正在申请…';
  resultDiv.style.display = 'none';

  const body = {
    node_id:  state.selectedNodeId,
    cpu:      state.cpu,
    memory:   state.memory,
    mem_unit: state.memUnit,
    device_num: state.device_num,
    username: username,
    password: password,
  };

  try {
    const resp = await fetch('/terminal/create', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await resp.json();

    if (resp.ok) {
      showToast('终端申请成功', 'success');
      resultDiv.className = 'result';
      resultDiv.innerHTML =
        `<strong>✓ 终端已创建</strong><br>` +
        (data.sandbox_id ? `ID: <code>${data.sandbox_id}</code><br>` : '') +
        (data.message ? `${data.message}<br>` : '');

      if (data.terminal_url) {
        resultDiv.innerHTML += `地址: <code>${data.terminal_url}</code>`;
        openTerminal(data.terminal_url);
      }
      resultDiv.style.display = 'block';
      fetchNodes();
    } else {
      showToast(data.error || '申请失败', 'error');
      resultDiv.className = 'result error';
      resultDiv.innerHTML = `<strong>✗ 申请失败</strong><br>${escapeHtml(data.error || '未知错误')}`;
      resultDiv.style.display = 'block';
    }
  } catch (err) {
    showToast('网络错误，请稍后重试', 'error');
    resultDiv.className = 'result error';
    resultDiv.innerHTML = `<strong>✗ 网络错误</strong><br>${escapeHtml(err.message)}`;
    resultDiv.style.display = 'block';
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = '申请终端';
  }
}

async function submitCommand() {
  const userId = cmdUserIdEl.value.trim();
  const password = cmdPasswordEl.value;
  const command = cmdInputEl.value.trim();
  if (!userId)   { showToast('请输入用户标识', 'error'); return; }
  if (!password) { showToast('请输入密码', 'error'); return; }
  if (!command)  { showToast('请输入命令', 'error'); return; }

  submitBtn.disabled = true;
  submitBtn.textContent = '提交中…';
  resultDiv.style.display = 'none';

  const body = {
    node_id:    state.selectedNodeId,
    user_id:    userId,
    password:   password,
    command:    command,
    cpu:        state.cpu,
    memory:     state.memory,
    mem_unit:   state.memUnit,
    device_num: state.device_num,
  };

  try {
    const resp = await fetch('/command/run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await resp.json();

    if (resp.ok) {
      showToast(`任务已提交，队列位置 #${data.position}`, 'success');
      cmdInputEl.value = '';
      resultDiv.className = 'result';
      resultDiv.innerHTML = `<strong>✓ 已提交</strong><br>任务ID: <code>${escapeHtml(data.task_id)}</code><br>队列位置: #${data.position}`;
      resultDiv.style.display = 'block';

      // Refresh queue immediately
      fetchQueue();
    } else {
      showToast(data.error || '提交失败', 'error');
      resultDiv.className = 'result error';
      resultDiv.innerHTML = `<strong>✗ 提交失败</strong><br>${escapeHtml(data.error || '未知错误')}`;
      resultDiv.style.display = 'block';
    }
  } catch (err) {
    showToast('网络错误: ' + err.message, 'error');
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = '提交命令';
  }
}

// ═══════════════════════════════════════════════════════════════
// Node management modal
// ═══════════════════════════════════════════════════════════════

const manageModal    = document.getElementById('manageModal');
const manageNodesBtn = document.getElementById('manageNodesBtn');
const modalClose     = document.getElementById('modalClose');
const configNodeList = document.getElementById('configNodeList');
const configNodeCount = document.getElementById('configNodeCount');
const newNodeName    = document.getElementById('newNodeName');
const newNodeHost    = document.getElementById('newNodeHost');
const newNodePort    = document.getElementById('newNodePort');
const addNodeBtn     = document.getElementById('addNodeBtn');

function openManageModal() {
  manageModal.style.display = '';
  fetchConfigNodes();
}

function closeManageModal() {
  manageModal.style.display = 'none';
}

manageNodesBtn.addEventListener('click', openManageModal);
modalClose.addEventListener('click', closeManageModal);
manageModal.addEventListener('click', (e) => {
  if (e.target === manageModal) closeManageModal();
});

async function fetchConfigNodes() {
  try {
    const resp = await fetch('/nodes/config');
    const data = await resp.json();
    const nodes = data.nodes || [];
    configNodeCount.textContent = `${nodes.length} 个`;
    renderConfigNodes(nodes);
  } catch (err) {
    configNodeList.innerHTML = `<div style="color:var(--danger);font-size:13px;text-align:center;padding:12px">加载失败: ${err.message}</div>`;
  }
}

function renderConfigNodes(nodes) {
  if (nodes.length === 0) {
    configNodeList.innerHTML = '<div style="color:var(--sub);font-size:13px;text-align:center;padding:12px">无已配置节点</div>';
    return;
  }
  configNodeList.innerHTML = nodes.map(n => `
    <div class="modal-node-item">
      <span class="node-name">${escapeHtml(n.name)}</span>
      <span class="node-addr">${escapeHtml(n.host)}:${n.port}</span>
      <button class="modal-delete-btn" data-name="${escapeHtml(n.name)}" title="删除节点">×</button>
    </div>
  `).join('');

  configNodeList.querySelectorAll('.modal-delete-btn').forEach(btn => {
    btn.addEventListener('click', async () => {
      const name = btn.dataset.name;
      if (!confirm(`确定要删除节点 "${name}" 吗？`)) return;
      await removeConfigNode(name);
    });
  });
}

async function addConfigNode() {
  const name = newNodeName.value.trim();
  const host = newNodeHost.value.trim();
  const port = parseInt(newNodePort.value.trim(), 10);

  if (!name) { showToast('请输入节点名称', 'error'); return; }
  if (!host) { showToast('请输入 host', 'error'); return; }
  if (isNaN(port) || port < 1 || port > 65535) { showToast('端口必须在 1-65535 之间', 'error'); return; }

  addNodeBtn.disabled = true;
  addNodeBtn.textContent = '…';

  try {
    const resp = await fetch('/nodes/config/add', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name, host, port }),
    });
    const data = await resp.json();
    if (resp.ok) {
      showToast(data.message, 'success');
      newNodeName.value = '';
      newNodeHost.value = '';
      newNodePort.value = '';
      fetchConfigNodes();
      fetchNodes(); // 刷新主节点列表
    } else {
      showToast(data.error || '添加失败', 'error');
    }
  } catch (err) {
    showToast('网络错误: ' + err.message, 'error');
  } finally {
    addNodeBtn.disabled = false;
    addNodeBtn.textContent = '添加';
  }
}

async function removeConfigNode(name) {
  try {
    const resp = await fetch('/nodes/config/remove', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    });
    const data = await resp.json();
    if (resp.ok) {
      showToast(data.message, 'success');
      fetchConfigNodes();
      fetchNodes(); // 刷新主节点列表
    } else {
      showToast(data.error || '删除失败', 'error');
    }
  } catch (err) {
    showToast('网络错误: ' + err.message, 'error');
  }
}

addNodeBtn.addEventListener('click', addConfigNode);
// Enter key in port field triggers add
newNodePort.addEventListener('keydown', (e) => {
  if (e.key === 'Enter') addConfigNode();
});

// ═══════════════════════════════════════════════════════════════
// Init
// ═══════════════════════════════════════════════════════════════

// Initial queue load if node is selected
if (state.selectedNodeId) {
  fetchQueue();
}
