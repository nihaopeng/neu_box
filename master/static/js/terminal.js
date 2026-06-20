// Terminal functions

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
      const errMsg = data.error || '申请失败';
      const errDetail = data.details ? `<br><span style="font-size:12px;color:#8e8e93">${escapeHtml(data.details)}</span>` : '';
      showToast(errMsg, 'error');
      resultDiv.className = 'result error';
      resultDiv.innerHTML = `<strong>✗ ${escapeHtml(errMsg)}</strong>${errDetail}`;
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
