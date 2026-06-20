// Command queue & log viewer

// Queue
// ═══════════════════════════════════════════════════════════════

function renderQueue(data) {
  const queue = data.queue || [];
  if (queue.length === 0) {
    queueList.innerHTML = '<div class="queue-empty">队列为空</div>';
    queueBatchBar.style.display = 'none';
    return;
  }

  queueList.innerHTML = queue.map(task => {
    const isRunning = task.status === 'running';
    const posText = isRunning ? '▶' : (task.position || '?');
    const isDone = task.status === 'completed' || task.status === 'failed';
    const clickable = isDone;

    return `
      <div class="queue-item ${isRunning ? 'running' : ''} ${clickable ? 'clickable' : ''}"
           data-task-id="${task.task_id}"
           data-user-id="${escapeHtml(task.user_id)}"
           title="${clickable ? '点击查看日志' : ''}">
        <input type="checkbox" class="queue-check" data-task-id="${task.task_id}" title="选择">
        <span class="queue-pos">${posText}</span>
        <span class="queue-user" title="${escapeHtml(task.user_id)}">${escapeHtml(task.user_id)}</span>
        <span class="queue-cmd" title="${escapeHtml(task.command)}">${escapeHtml(task.command)}</span>
        <span class="queue-status ${task.status}">${statusLabel(task.status)}</span>
      </div>`;
  }).join('');

  // Bind click: only completed/failed → view log (ignore clicks on checkbox)
  queueList.querySelectorAll('.queue-item.clickable').forEach(item => {
    item.addEventListener('click', (e) => {
      if (e.target.classList.contains('queue-check')) return;
      // 选中高亮
      queueList.querySelectorAll('.queue-item.active').forEach(el => el.classList.remove('active'));
      item.classList.add('active');
      const taskId = item.dataset.taskId;
      if (taskId) viewTaskLog(taskId);
    });
  });

  // Checkbox change → update batch bar
  queueList.querySelectorAll('.queue-check').forEach(cb => {
    cb.addEventListener('change', updateBatchBar);
    // Stop propagation so clicking checkbox doesn't trigger viewTaskLog
    cb.addEventListener('click', e => e.stopPropagation());
  });

  // Select-all
  const selectAllCb = document.getElementById('queueSelectAll');
  if (selectAllCb) selectAllCb.checked = false;
  queueBatchBar.style.display = 'none';
}

function getCheckedTaskIds() {
  const checked = queueList.querySelectorAll('.queue-check:checked');
  return Array.from(checked).map(cb => cb.dataset.taskId);
}

function updateBatchBar() {
  const count = getCheckedTaskIds().length;
  const selectAllCb = document.getElementById('queueSelectAll');
  const countEl = document.getElementById('queueBatchCount');
  if (count > 0) {
    queueBatchBar.style.display = '';
    if (countEl) countEl.textContent = `已选 ${count} 项`;
    // Update select-all state
    const allCbs = queueList.querySelectorAll('.queue-check');
    if (selectAllCb) selectAllCb.checked = (count === allCbs.length);
  } else {
    queueBatchBar.style.display = 'none';
    if (selectAllCb) selectAllCb.checked = false;
  }
}

document.getElementById('queueSelectAll').addEventListener('change', function() {
  const checked = this.checked;
  queueList.querySelectorAll('.queue-check').forEach(cb => {
    cb.checked = checked;
  });
  updateBatchBar();
});

document.getElementById('queueBatchDeleteBtn').addEventListener('click', async () => {
  const ids = getCheckedTaskIds();
  if (ids.length === 0) return;
  if (!confirm(`确定删除 ${ids.length} 个任务吗？（运行中的任务将被强制终止）`)) return;

  try {
    const resp = await fetch('/command/tasks/delete', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ node_id: state.selectedNodeId, task_ids: ids }),
    });
    const data = await resp.json();
    if (resp.ok) {
      showToast(data.message || `已删除`, 'success');
      fetchQueue();
    } else {
      showToast(data.error || '删除失败', 'error');
    }
  } catch (err) {
    showToast('网络错误: ' + err.message, 'error');
  }
});

function statusLabel(s) {
  const map = { queued: '排队中', running: '执行中', completed: '已完成', failed: '失败' };
  return map[s] || s;
}

async function fetchQueue() {
  if (!state.selectedNodeId) {
    queueList.innerHTML = '<div class="queue-empty">选择节点后刷新</div>';
    queueBatchBar.style.display = 'none';
    return;
  }

  try {
    const resp = await fetch(`/command/queue?node_id=${encodeURIComponent(state.selectedNodeId)}`);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const data = await resp.json();
    renderQueue(data);
  } catch (err) {
    queueList.innerHTML = `<div class="queue-empty">加载失败: ${err.message}</div>`;
    queueBatchBar.style.display = 'none';
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

  logPlaceholder.style.display = 'none';
  logContent.style.display = '';
  logContent.innerHTML = '<div style="color:#636366">加载中…</div>';
  logActions.style.display = 'none';

  try {
    const params = new URLSearchParams({
      node_id: state.selectedNodeId,
    });
    const resp = await fetch(`/command/result/${taskId}?${params}`);
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
    const task = await resp.json();

    // 存下来，供「保存为实验记录」使用
    _currentTaskData = {
      task_id: task.task_id,
      node_id: state.selectedNodeId,
      command: task.command,
      user_id: task.user_id,
      cpu: task.cpu,
      mem: task.mem,
      device_num: task.device_num,
      devices: task.devices,
      status: task.status,
      // 完整任务日志，保存实验时直接传给后端持久化
      task_result: task.result ? {
        status: task.status,
        command: task.command,
        cpu: task.cpu,
        mem: task.mem,
        device_num: task.device_num,
        created_at: task.created_at,
        result: task.result,
      } : null,
    };

    // 密码错误 → 无权限
    if (task.permission_denied) {
      logContent.innerHTML = '<div style="color:#ff5f57;font-size:16px;text-align:center;padding:40px">🔒 无权限<br><span style="font-size:13px;color:#8e8e93">密码不正确</span></div>';
      logActions.style.display = 'none';
      return;
    }

    if (task.error) {
      logContent.innerHTML = `<div style="color:#ff5f57">错误: ${escapeHtml(task.error)}</div>`;
      logActions.style.display = 'none';
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
    // 显示「保存为实验记录」按钮（仅当任务已完成或失败时）
    if (task.status === 'completed' || task.status === 'failed') {
      logActions.style.display = '';
    } else {
      logActions.style.display = 'none';
    }

  } catch (err) {
    logContent.innerHTML = `<div style="color:#ff5f57">加载失败: ${err.message}</div>`;
    logActions.style.display = 'none';
  }
}


async function submitCommand() {
  const userId = cmdUserIdEl.value.trim();
  const command = cmdInputEl.value.trim();
  if (!userId)   { showToast('请输入用户标识', 'error'); return; }
  if (!command)  { showToast('请输入命令', 'error'); return; }

  submitBtn.disabled = true;
  submitBtn.textContent = '提交中…';
  resultDiv.style.display = 'none';

  const body = {
    node_id:    state.selectedNodeId,
    user_id:    userId,
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
