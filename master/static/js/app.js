// ── State ──────────────────────────────────────────────────────
const state = {
  selectedNodeId: null,
  cpu:     0,
  memory:  0,
  memUnit: 'GB',
  device_num: 0,
};

const limits = {
  cpu:    { min: 0,   max: 64   },
  memory: { min: 0,   max: 256  },
  device_num: { min: 0, max: 16 },
};

// ── DOM refs ──────────────────────────────────────────────────
const form              = document.getElementById('terminalForm');
const submitBtn         = document.getElementById('submitBtn');
const resultDiv         = document.getElementById('result');
const toast             = document.getElementById('toast');
const memUnitEl         = document.getElementById('memUnit');
const terminalPanel     = document.getElementById('terminalPanel');
const terminalIframe    = document.getElementById('terminalIframe');
const terminalUrlEl     = document.getElementById('terminalUrl');
const terminalPlaceholder = document.getElementById('terminalPlaceholder');
const terminalClose     = document.getElementById('terminalClose');
const nodeList          = document.getElementById('nodeList');
const nodeCount         = document.getElementById('nodeCount');
const usernameInput     = document.getElementById('usernameInput');
const passwordInput     = document.getElementById('passwordInput');

// ── Formatting helpers ────────────────────────────────────────

function formatBytes(bytes) {
  if (!bytes || bytes === 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, val = bytes;
  while (val >= 1024 && i < units.length - 1) { val /= 1024; i++; }
  return val >= 100 ? `${Math.round(val)} ${units[i]}` : `${val.toFixed(1)} ${units[i]}`;
}

function isIdlePercent(cpuIdle) {
  // worker /status 返回的 idle_cpu 是百分比（float, 0-100），
  // 而旧接口可能返回空闲核心数（int）。用小数部分来区分。
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
  if (isIdlePercent(cpuIdle)) {
    return 100 - cpuIdle;
  }
  return ((cpuTotal - cpuIdle) / cpuTotal) * 100;
}

// ── Node rendering ────────────────────────────────────────────

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
        <span style="font-size:11px;color:var(--sub)">节点离线</span>
      </div>
      `}
    </div>`;
}

function renderNodeCards(nodes) {
  nodeList.innerHTML = nodes.map(renderNodeCard).join('');

  // 绑定点击事件
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
  submitBtn.disabled = false;
}

// ── Node fetching ────────────────────────────────────────────

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

    // 若当前选中节点不在列表中，且第一个是在线节点则自动选中
    const currentSelected = nodes.find(n => n.node_id === state.selectedNodeId);
    if (!currentSelected) {
      const firstOnline = nodes.find(n => n.status === 'online');
      if (firstOnline) {
        state.selectedNodeId = firstOnline.node_id;
      }
    }

    renderNodeCards(nodes);
    submitBtn.disabled = !state.selectedNodeId;

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

// 页面加载时获取节点列表，之后每 60 秒自动刷新
fetchNodes();
setInterval(fetchNodes, 60000);

// ── Helpers ───────────────────────────────────────────────────

function updateButtons(stepper, field) {
  const lim = limits[field];
  const cur = state[field];
  stepper.querySelector('[data-action=down]').disabled = cur <= lim.min;
  stepper.querySelector('[data-action=up]').disabled   = cur >= lim.max;
}

function showToast(msg, type) {
  toast.textContent = msg;
  toast.className = `toast ${type}`;
  toast.style.display = 'block';
  setTimeout(() => { toast.style.display = 'none'; }, 3000);
}

function openTerminal(url) {
  terminalIframe.src = url;
  terminalUrlEl.textContent = url;
  terminalPanel.classList.add('active');
  submitBtn.textContent = '重新申请';
  submitBtn.classList.add('has-terminal');
}

function closeTerminal() {
  terminalIframe.src = '';
  terminalUrlEl.textContent = '';
  terminalPanel.classList.remove('active');
  submitBtn.textContent = '申请终端';
  submitBtn.classList.remove('has-terminal');
}

// ── Stepper events ────────────────────────────────────────────

// ── Value display helpers ──────────────────────────────────────

function setValDisplay(el, field, val) {
  if (val === 0 && field !== 'device_num') {
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
  if (isNaN(n) || n < 0) return state[field]; // 非法输入，回退
  return n;
}

// ── Stepper events ────────────────────────────────────────────

document.querySelectorAll('.stepper').forEach(stepper => {
  const field = stepper.dataset.field;
  const valEl = stepper.querySelector('.value');

  // 按钮点击：增减
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

  // 手动输入提交：blur 或 Enter
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

  // 初始化显示
  setValDisplay(valEl, field, state[field]);
  updateButtons(stepper, field);
});

// ── Memory unit toggle ────────────────────────────────────────

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

// ── Terminal close ────────────────────────────────────────────

terminalClose.addEventListener('click', closeTerminal);

// ── Manual refresh ────────────────────────────────────────────

const refreshBtn = document.getElementById('refreshBtn');
refreshBtn.addEventListener('click', () => {
  refreshBtn.classList.add('spinning');
  fetchNodes().finally(() => {
    refreshBtn.classList.remove('spinning');
  });
});

// ── Form submit ───────────────────────────────────────────────

form.addEventListener('submit', async (e) => {
  e.preventDefault();

  if (!state.selectedNodeId) {
    showToast('请先选择一个节点', 'error');
    return;
  }

  submitBtn.disabled = true;
  submitBtn.textContent = '正在申请…';
  resultDiv.style.display = 'none';

  const body = {
    node_id:  state.selectedNodeId,
    cpu:      state.cpu,
    memory:   state.memory,
    mem_unit: state.memUnit,
    device_num: state.device_num,
    username: usernameInput.value.trim(),
    password: passwordInput.value,
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

      // 立即刷新节点状态，反映最新的资源占用
      fetchNodes();
    } else {
      showToast(data.error || '申请失败', 'error');
      resultDiv.className = 'result error';
      resultDiv.innerHTML =
        `<strong>✗ 申请失败</strong><br>${data.error || data.message || '未知错误'}`;
      resultDiv.style.display = 'block';
    }
  } catch (err) {
    showToast('网络错误，请稍后重试', 'error');
    resultDiv.className = 'result error';
    resultDiv.innerHTML = `<strong>✗ 网络错误</strong><br>${err.message}`;
    resultDiv.style.display = 'block';
  } finally {
    submitBtn.disabled = false;
    submitBtn.textContent = terminalPanel.classList.contains('active')
      ? '重新申请'
      : '申请终端';
  }
});
