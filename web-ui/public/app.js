let settingsLoadedOnce = false;
let currentAuth = { logged_in: false, user: null };
let cachedOsWims = [];
let passwordModalForced = false;

function switchTab(tabName) {
  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.tab === tabName);
  });
  document.querySelectorAll('.tab-panel').forEach(panel => {
    panel.classList.toggle('active', panel.id === `tab-${tabName}`);
  });
}

function showToast(msg) {
  const toast = document.getElementById('toast');
  toast.textContent = msg;
  toast.classList.remove('hidden');
  setTimeout(() => toast.classList.add('hidden'), 3500);
}

async function refreshOverview() {
  const categoryEl = document.getElementById('log-category');
  const category = categoryEl ? categoryEl.value : 'ALL';

  try {
    const res = await fetch(`/api/overview?category=${encodeURIComponent(category)}`);
    const data = await res.json();

    const badge = document.getElementById('backend-badge');
    if (data.error) {
      badge.textContent = 'Go Server Offline';
      badge.className = 'status-pill offline';
      return;
    }

    badge.textContent = 'Go Server Online';
    badge.className = 'status-pill online';

    applyAuthState(data.auth || { logged_in: false, user: null });
    renderStats(data);
    renderTransfers(data.transfers || []);
    renderManufacturerAnalytics(data.mfg_breakdown || {}, (data.devices || []).length);
    renderDevices(data.devices || [], data.os_files || []);

    if (currentAuth.logged_in) {
      renderImages(data);
      renderRules(data.rules || []);
      renderNodes(data.nodes || [], data.config || {});
      renderLogs(data.logs || []);
      renderSettingsAndUsers(data);
    }
  } catch (err) {
    console.error('Failed to poll overview:', err);
  }
}

function applyAuthState(auth) {
  currentAuth = auth;
  const isLogged = !!auth.logged_in;
  const user = auth.user || {};
  const canImages = isLogged && (user.role === 'admin' || user.can_manage_images);
  const canSettings = isLogged && (user.role === 'admin' || user.can_manage_settings);

  document.getElementById('view-only-banner').classList.toggle('hidden', isLogged);
  document.getElementById('main-tabs').classList.toggle('hidden', !isLogged);
  document.getElementById('login-btn').classList.toggle('hidden', isLogged);
  document.getElementById('user-pill').classList.toggle('hidden', !isLogged);
  document.getElementById('simulate-btn').classList.toggle('hidden', !isLogged);
  document.getElementById('tab-btn-setup').classList.toggle('hidden', !canSettings);

  if (!isLogged) {
    switchTab('dashboard');
  } else {
    document.getElementById('current-user-label').textContent = `${user.username} (${user.role})`;
    if (user.must_change_password && !passwordModalForced) {
      passwordModalForced = true;
      openPasswordModal(true);
    }
  }

  document.querySelectorAll('.img-perm-only').forEach(el => {
    el.classList.toggle('hidden', !canImages);
  });
  document.querySelectorAll('.settings-perm-only').forEach(el => {
    el.classList.toggle('hidden', !canSettings);
  });
}

function renderStats(data) {
  const cfg = data.config || {};
  document.getElementById('node-name-badge').textContent = cfg.node_name || 'Primary Node';
  document.getElementById('stat-server-ip').textContent = cfg.server_ip || '--';
  document.getElementById('stat-ports').textContent = `TFTP :${cfg.tftp_port || 69} | HTTP :${cfg.http_port || 80} | Web :${cfg.ruby_web_port || 4567}`;

  const winReady = !!data.winpe_ready;
  const osCount = (data.os_files || []).length;
  const winEl = document.getElementById('stat-winpe-status');
  winEl.textContent = winReady ? 'WinPE Ready' : 'Needs boot.wim';
  winEl.style.color = winReady ? '#34d399' : '#fbbf24';
  document.getElementById('stat-default-wim').textContent = `${osCount} Configured OS WIM(s) | Base: ${cfg.default_wim || 'boot.wim'}`;

  const activeDl = (data.transfers || []).filter(t => t.status === 'downloading').length;
  document.getElementById('stat-active-transfers').textContent = activeDl;
  document.getElementById('stat-device-count').textContent = (data.devices || []).length;
}

function renderManufacturerAnalytics(mfgMap, totalDevices) {
  const container = document.getElementById('mfg-analytics-container');
  const entries = Object.entries(mfgMap);
  if (!entries.length || totalDevices === 0) {
    container.innerHTML = '<p class="empty-state">No hardware telemetry collected yet.</p>';
    return;
  }

  container.innerHTML = entries.map(([mfg, count]) => {
    const pct = Math.round((count / totalDevices) * 100);
    return `
      <div class="transfer-item">
        <div class="transfer-top">
          <strong>${escapeHtml(mfg)}</strong>
          <span><strong>${count}</strong> device(s) <span class="muted">(${pct}%)</span></span>
        </div>
        <div class="progress-track">
          <div class="progress-fill" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }).join('');
}

function renderTransfers(transfers) {
  const container = document.getElementById('transfers-container');
  if (!transfers.length) {
    container.innerHTML = '<p class="empty-state">No active image downloads right now.</p>';
    return;
  }

  container.innerHTML = transfers.map(t => {
    const pct = (t.progress_pct || 0).toFixed(1);
    const speed = (t.speed_mbps || 0).toFixed(1);
    const devLabel = t.device_model ? `${t.device_model} (${t.device_mac || t.client_ip})` : (t.device_mac || t.client_ip);
    const statusBadge = t.status === 'completed'
      ? '<span class="badge badge-ok">Completed</span>'
      : t.status === 'downloading'
        ? '<span class="badge badge-info">Downloading</span>'
        : '<span class="badge badge-warn">Interrupted</span>';

    return `
      <div class="transfer-item">
        <div class="transfer-top">
          <div>
            <strong>${escapeHtml(t.filename)}</strong> &rarr;
            <span>${escapeHtml(devLabel)}</span>
            <span class="muted">[IP: ${escapeHtml(t.client_ip)}]</span>
          </div>
          <div>
            ${statusBadge}
            <strong>${pct}%</strong>
            <span class="muted">(${speed} MB/s)</span>
          </div>
        </div>
        <div class="progress-track">
          <div class="progress-fill" style="width: ${pct}%"></div>
        </div>
      </div>
    `;
  }).join('');
}

function renderDevices(devices, osFiles) {
  cachedOsWims = osFiles.map(f => f.name);
  const dashTbody = document.getElementById('dashboard-devices-tbody');
  const fullTbody = document.getElementById('devices-tbody');

  if (!devices.length) {
    dashTbody.innerHTML = '<tr><td colspan="8" class="empty-cell">No devices discovered yet.</td></tr>';
    if (fullTbody) fullTbody.innerHTML = '<tr><td colspan="8" class="empty-cell">No PXE devices have checked in yet.</td></tr>';
    return;
  }

  dashTbody.innerHTML = devices.slice(0, 8).map(d => {
    const matchedBadge = d.matched_os_wim
      ? `<span class="badge badge-ok">${escapeHtml(d.matched_os_wim)}</span> <small class="muted">${escapeHtml(d.matched_rule)}</small>`
      : '<span class="muted">Manual / Prompt</span>';
    const nodeLabel = d.node_name || 'Local Node';

    return `
      <tr>
        <td><span class="badge badge-info">${escapeHtml(nodeLabel)}</span></td>
        <td><code>${escapeHtml(d.mac)}</code></td>
        <td>${escapeHtml(d.ip)}</td>
        <td><strong>${escapeHtml(d.manufacturer)}</strong> ${escapeHtml(d.model)}</td>
        <td><code>${escapeHtml(d.serial)}</code> <small class="muted">(BIOS: ${escapeHtml(d.bios_version || '-')})</small></td>
        <td>${matchedBadge}</td>
        <td><span class="badge badge-info">${escapeHtml(d.current_status)}</span></td>
        <td class="muted">${escapeHtml(d.last_seen)}</td>
      </tr>
    `;
  }).join('');

  if (!fullTbody) return;

  fullTbody.innerHTML = devices.map(d => {
    const matchedBadge = d.matched_os_wim
      ? `<span class="badge badge-ok">${escapeHtml(d.matched_os_wim)}</span><br><small class="muted">${escapeHtml(d.matched_rule)}</small>`
      : '<span class="muted">None</span>';
    const nodeLabel = d.node_name || 'Local Node';

    const osOptions = cachedOsWims.map(w => {
      const val = `os:${w}`;
      const sel = d.next_boot_action === val ? 'selected' : '';
      return `<option value="${escapeHtml(val)}" ${sel}>Deploy OS: ${escapeHtml(w)} (1x)</option>`;
    }).join('');

    return `
      <tr>
        <td><span class="badge badge-info">${escapeHtml(nodeLabel)}</span><br><small class="muted">${escapeHtml(d.last_seen)}</small></td>
        <td><code>${escapeHtml(d.mac)}</code><br><small class="muted">${escapeHtml(d.ip)}</small></td>
        <td><strong>${escapeHtml(d.manufacturer)}</strong><br>${escapeHtml(d.model)} <small class="muted">(${escapeHtml(d.arch)})</small></td>
        <td>SN: <code>${escapeHtml(d.serial)}</code><br><small class="muted">Asset: ${escapeHtml(d.asset_tag || '-')}</small></td>
        <td>BIOS: <code>${escapeHtml(d.bios_version || '-')}</code><br><small class="muted">NIC: ${escapeHtml(d.nic_chip || '-')}</small></td>
        <td>${matchedBadge}</td>
        <td>
          <span class="badge badge-info">${escapeHtml(d.current_status)}</span>
          ${d.progress_pct > 0 ? `<small class="muted">(${d.progress_pct}%)</small>` : ''}
        </td>
        <td>
          <select class="queue-select" onchange="queueDeviceBoot('${escapeJs(d.mac)}', this.value)">
            <option value="menu" ${d.next_boot_action === 'menu' ? 'selected' : ''}>Menu (Default: Exit to Disk)</option>
            <option value="winpe" ${d.next_boot_action === 'winpe' ? 'selected' : ''}>Boot WinPE Once</option>
            ${osOptions}
            <option value="linux" ${d.next_boot_action === 'linux' ? 'selected' : ''}>Boot Linux Once</option>
            <option value="exit" ${d.next_boot_action === 'exit' ? 'selected' : ''}>Always Skip to Local Disk</option>
          </select>
        </td>
      </tr>
    `;
  }).join('');
}

function renderImages(data) {
  const user = currentAuth.user || {};
  const canImages = user.role === 'admin' || user.can_manage_images;

  const osFiles = data.os_files || [];
  const datalist = document.getElementById('os-wim-datalist');
  if (datalist) {
    datalist.innerHTML = osFiles.map(f => `<option value="${escapeHtml(f.name)}"></option>`).join('');
  }

  const osTbody = document.getElementById('os-files-tbody');
  if (!osFiles.length) {
    osTbody.innerHTML = '<tr><td colspan="4" class="empty-cell">No OS deployment WIMs in http_files/images/os/ yet.</td></tr>';
  } else {
    osTbody.innerHTML = osFiles.map(f => `
      <tr>
        <td><strong>${escapeHtml(f.name)}</strong></td>
        <td>${escapeHtml(f.size_human)}</td>
        <td>
          <input type="checkbox" ${f.sync_enabled ? 'checked' : ''} ${!canImages ? 'disabled' : ''}
            onchange="toggleSyncFlag('os', '${escapeJs(f.name)}', this.checked)">
          <small class="muted">Sync</small>
        </td>
        <td>
          ${canImages ? `<button class="btn btn-sm btn-danger" onclick="deleteImage('os', '${escapeJs(f.name)}')">Remove</button>` : '<span class="muted">Read-Only</span>'}
        </td>
      </tr>
    `).join('');
  }

  const winBadge = document.getElementById('winpe-ready-badge');
  winBadge.textContent = data.winpe_ready ? 'WinPE Ready' : 'Missing Default WIM';
  winBadge.className = `badge ${data.winpe_ready ? 'badge-ok' : 'badge-warn'}`;

  const linuxBadge = document.getElementById('linux-ready-badge');
  linuxBadge.textContent = data.linux_ready ? 'Linux Kernel Ready' : 'Placeholder Active';
  linuxBadge.className = `badge ${data.linux_ready ? 'badge-ok' : 'badge-info'}`;

  const winFiles = data.windows_files || [];
  const winTbody = document.getElementById('win-files-tbody');
  if (!winFiles.length) {
    winTbody.innerHTML = '<tr><td colspan="5" class="empty-cell">No files in http_files/images/winpe/</td></tr>';
  } else {
    winTbody.innerHTML = winFiles.map(f => {
      let roleBadge = '<span class="badge badge-info">Support File</span>';
      if (f.protected) roleBadge = '<span class="badge badge-ok">Core Bootloader</span>';
      if (f.is_wim) {
        roleBadge = f.is_default_wim
          ? '<span class="badge badge-ok">★ Active Boot WIM</span>'
          : '<span class="badge badge-info">WinPE WIM</span>';
      }

      let actions = '';
      if (canImages) {
        if (f.is_wim && !f.is_default_wim) {
          actions += `<button class="btn btn-sm btn-secondary" onclick="setDefaultWim('${escapeJs(f.name)}')">Set Default</button> `;
        }
        if (!f.protected) {
          actions += `<button class="btn btn-sm btn-danger" onclick="deleteImage('windows', '${escapeJs(f.name)}')">Remove</button>`;
        } else {
          actions += '<span class="muted">Protected</span>';
        }
      } else {
        actions = '<span class="muted">Read-Only</span>';
      }

      return `
        <tr>
          <td><strong>${escapeHtml(f.name)}</strong></td>
          <td>${escapeHtml(f.size_human)}</td>
          <td>${roleBadge}</td>
          <td>
            <input type="checkbox" ${f.sync_enabled ? 'checked' : ''} ${!canImages ? 'disabled' : ''}
              onchange="toggleSyncFlag('windows', '${escapeJs(f.name)}', this.checked)">
          </td>
          <td>${actions}</td>
        </tr>
      `;
    }).join('');
  }

  const linuxFiles = data.linux_files || [];
  const linuxTbody = document.getElementById('linux-files-tbody');
  if (!linuxFiles.length) {
    linuxTbody.innerHTML = '<tr><td colspan="4" class="empty-cell">No Linux kernel/initrd uploaded yet.</td></tr>';
  } else {
    linuxTbody.innerHTML = linuxFiles.map(f => `
      <tr>
        <td><strong>${escapeHtml(f.name)}</strong></td>
        <td>${escapeHtml(f.size_human)}</td>
        <td>
          <input type="checkbox" ${f.sync_enabled ? 'checked' : ''} ${!canImages ? 'disabled' : ''}
            onchange="toggleSyncFlag('linux', '${escapeJs(f.name)}', this.checked)">
        </td>
        <td>
          ${canImages ? `<button class="btn btn-sm btn-danger" onclick="deleteImage('linux', '${escapeJs(f.name)}')">Remove</button>` : '<span class="muted">Read-Only</span>'}
        </td>
      </tr>
    `).join('');
  }
}

function renderRules(rules) {
  const user = currentAuth.user || {};
  const canImages = user.role === 'admin' || user.can_manage_images;
  const tbody = document.getElementById('rules-tbody');
  if (!rules.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="empty-cell">No BIOS Auto-Image rules configured yet.</td></tr>';
    return;
  }
  tbody.innerHTML = rules.map(r => `
    <tr>
      <td><strong>${escapeHtml(r.name)}</strong></td>
      <td><code>${escapeHtml(r.manufacturer_match)}</code></td>
      <td><code>${escapeHtml(r.model_match)}</code></td>
      <td><span class="badge badge-ok">${escapeHtml(r.target_wim)}</span></td>
      <td>
        ${canImages ? `<button class="btn btn-sm btn-danger" onclick="deleteRule(${r.id})">Delete</button>` : '<span class="muted">-</span>'}
      </td>
    </tr>
  `).join('');
}

function renderNodes(nodes, cfg) {
  const syncKeyEl = document.getElementById('this-node-sync-key');
  if (syncKeyEl && cfg.sync_secret) {
    syncKeyEl.textContent = cfg.sync_secret;
  }
  const tbody = document.getElementById('nodes-tbody');
  if (!nodes.length) {
    tbody.innerHTML = '<tr><td colspan="5" class="empty-cell">No remote warehouse nodes registered yet.</td></tr>';
    return;
  }
  tbody.innerHTML = nodes.map(n => `
    <tr>
      <td><strong>${escapeHtml(n.name)}</strong></td>
      <td><code>${escapeHtml(n.url)}</code></td>
      <td class="muted">${escapeHtml(n.last_sync_at)}</td>
      <td>
        <span class="badge badge-info">${escapeHtml(n.last_sync_status)}</span>
        ${n.sync_progress_pct > 0 ? `<small class="muted">(${n.sync_progress_pct}%)</small>` : ''}
      </td>
      <td>
        <button class="btn btn-sm btn-danger settings-perm-only" onclick="deleteNode(${n.id})">Remove</button>
      </td>
    </tr>
  `).join('');
}

function renderLogs(logs) {
  const tbody = document.getElementById('logs-tbody');
  if (!logs.length) {
    tbody.innerHTML = '<tr><td colspan="6" class="empty-cell">No log events matching filter.</td></tr>';
    return;
  }

  tbody.innerHTML = logs.map(l => {
    const levelClass = l.level === 'ERROR' ? 'badge-warn' : 'badge-info';
    return `
      <tr>
        <td class="muted">${escapeHtml(l.created_at)}</td>
        <td><span class="badge ${levelClass}">${escapeHtml(l.level)}</span></td>
        <td><strong>${escapeHtml(l.category)}</strong></td>
        <td>${escapeHtml(l.client_ip || '-')}</td>
        <td><code>${escapeHtml(l.device_mac || '-')}</code></td>
        <td>${escapeHtml(l.message)}</td>
      </tr>
    `;
  }).join('');
}

function renderSettingsAndUsers(data) {
  const cfg = data.config || {};
  const ifaces = data.interfaces || [];
  const users = data.users || [];

  const ifaceContainer = document.getElementById('interfaces-list');
  if (ifaces.length) {
    ifaceContainer.innerHTML = ifaces.map(i => `
      <button type="button" class="iface-pill" onclick="document.getElementById('cfg-server-ip').value='${escapeJs(i.ip)}'">
        ${escapeHtml(i.name)}: <strong>${escapeHtml(i.ip)}</strong> (${escapeHtml(i.mac)})
      </button>
    `).join('');
  }

  const usersTbody = document.getElementById('users-tbody');
  if (usersTbody) {
    usersTbody.innerHTML = users.map(u => `
      <tr>
        <td><strong>${escapeHtml(u.username)}</strong></td>
        <td><span class="badge badge-info">${escapeHtml(u.role)}</span></td>
        <td>${u.can_manage_images ? '<span class="badge badge-ok">Allowed</span>' : '<span class="muted">No</span>'}</td>
        <td>${u.must_change_password ? '<span class="badge badge-warn">Reset on Login</span>' : '<span class="badge badge-ok">Active</span>'}</td>
        <td><button class="btn btn-sm btn-danger" onclick="deleteUser(${u.id})">Delete</button></td>
      </tr>
    `).join('');
  }

  if (!settingsLoadedOnce && cfg.server_ip) {
    document.getElementById('cfg-node-name').value = cfg.node_name || 'Primary-Imaging-Node';
    document.getElementById('cfg-server-ip').value = cfg.server_ip || '';
    document.getElementById('cfg-default-wim').value = cfg.default_wim || 'boot.wim';
    document.getElementById('cfg-tftp-port').value = cfg.tftp_port || 69;
    document.getElementById('cfg-http-port').value = cfg.http_port || 80;
    document.getElementById('cfg-ruby-port').value = cfg.ruby_web_port || 4567;
    document.getElementById('cfg-timeout').value = cfg.menu_timeout_ms || 10000;
    document.getElementById('cfg-linux-args').value = cfg.linux_kernel_args || '';
    document.getElementById('cfg-autolaunch').checked = !!cfg.auto_launch_winpe_installer;
    if (document.getElementById('cfg-sync-schedule')) {
      document.getElementById('cfg-sync-schedule').value = cfg.sync_schedule || '02:00';
      document.getElementById('cfg-sync-mbps').value = cfg.sync_max_mbps || 0;
      document.getElementById('cfg-sync-rules').checked = cfg.sync_rules_with_images !== false;
    }
    settingsLoadedOnce = true;
  }
}

function openLoginModal() {
  document.getElementById('login-modal').classList.remove('hidden');
  document.getElementById('login-username').focus();
}

function closeLoginModal() {
  document.getElementById('login-modal').classList.add('hidden');
}

async function submitLogin(event) {
  event.preventDefault();
  const username = document.getElementById('login-username').value.trim();
  const password = document.getElementById('login-password').value;

  const res = await fetch('/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Login failed: ${data.error}`);
    return;
  }
  closeLoginModal();
  document.getElementById('login-password').value = '';
  showToast(`Welcome, ${data.user.username}!`);
  refreshOverview();
}

async function logout() {
  await fetch('/auth/logout', { method: 'POST' });
  settingsLoadedOnce = false;
  passwordModalForced = false;
  showToast('Signed out (View-Only mode active)');
  refreshOverview();
}

function openPasswordModal(forced = false) {
  const modal = document.getElementById('password-modal');
  const cancelBtn = document.getElementById('password-cancel-btn');
  const titleEl = document.getElementById('password-modal-title');
  const descEl = document.getElementById('password-modal-desc');

  if (forced) {
    titleEl.textContent = 'First-Time Login: Change Default Password';
    descEl.textContent = 'For security, please replace your initial setup password before managing the server.';
    cancelBtn.classList.add('hidden');
  } else {
    titleEl.textContent = 'Change Your Password';
    descEl.textContent = 'Enter a new password for your account.';
    cancelBtn.classList.remove('hidden');
  }
  modal.classList.remove('hidden');
  document.getElementById('new-password-input').focus();
}

function closePasswordModal() {
  document.getElementById('password-modal').classList.add('hidden');
}

async function submitPasswordChange(event) {
  event.preventDefault();
  const newPassword = document.getElementById('new-password-input').value;
  const res = await fetch('/auth/change-password', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ new_password: newPassword })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
    return;
  }
  document.getElementById('new-password-input').value = '';
  closePasswordModal();
  showToast('Password updated successfully!');
  refreshOverview();
}

function onRoleSelectChange() {
  const role = document.getElementById('usr-role').value;
  const canImg = document.getElementById('usr-can-images');
  if (role === 'admin') {
    canImg.checked = true;
  } else if (role === 'viewer') {
    canImg.checked = false;
  }
}

async function saveUser(event) {
  event.preventDefault();
  const role = document.getElementById('usr-role').value;
  const payload = {
    username: document.getElementById('usr-username').value.trim(),
    password: document.getElementById('usr-password').value,
    role,
    can_manage_images: document.getElementById('usr-can-images').checked,
    can_manage_settings: role === 'admin',
    must_change_password: document.getElementById('usr-must-reset').checked
  };
  const res = await fetch('/users/save', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    document.getElementById('usr-username').value = '';
    document.getElementById('usr-password').value = '';
    showToast('Saved user account!');
    refreshOverview();
  }
}

async function deleteUser(id) {
  if (!confirm('Delete this user account?')) return;
  const res = await fetch('/users/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    showToast('Deleted user account');
    refreshOverview();
  }
}

async function uploadImage(event, category) {
  event.preventDefault();
  const inputMap = { os: 'os-file-input', windows: 'win-file-input', linux: 'linux-file-input' };
  const progMap = { os: 'os-upload-progress', windows: 'win-upload-progress', linux: 'linux-upload-progress' };
  const fileInput = document.getElementById(inputMap[category]);
  const progressEl = document.getElementById(progMap[category]);

  if (!fileInput.files.length) return;

  const formData = new FormData();
  formData.append('category', category);
  formData.append('file', fileInput.files[0]);

  progressEl.textContent = `Uploading ${fileInput.files[0].name}...`;
  progressEl.classList.remove('hidden');

  try {
    const res = await fetch('/images/upload', { method: 'POST', body: formData });
    const data = await res.json();
    if (data.error) {
      showToast(`Upload error: ${data.error}`);
    } else {
      showToast(`Uploaded ${fileInput.files[0].name} successfully!`);
      fileInput.value = '';
      refreshOverview();
    }
  } catch (err) {
    showToast(`Upload failed: ${err.message}`);
  } finally {
    progressEl.classList.add('hidden');
  }
}

async function deleteImage(category, filename) {
  if (!confirm(`Remove ${filename}?`)) return;
  const res = await fetch('/images/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ category, filename })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    showToast(`Deleted ${filename}`);
    refreshOverview();
  }
}

async function setDefaultWim(filename) {
  const res = await fetch('/images/default-wim', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ filename })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    document.getElementById('cfg-default-wim').value = filename;
    showToast(`Active WinPE boot WIM set to ${filename}`);
    refreshOverview();
  }
}

async function toggleSyncFlag(category, filename, enabled) {
  const res = await fetch('/images/sync-flag', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ category, filename, sync_enabled: enabled })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    showToast(`${filename} ${enabled ? 'flagged for warehouse sync' : 'removed from warehouse sync'}`);
    refreshOverview();
  }
}

async function createRule(event) {
  event.preventDefault();
  const payload = {
    name: document.getElementById('rule-name').value.trim(),
    manufacturer_match: document.getElementById('rule-mfg').value.trim(),
    model_match: document.getElementById('rule-model').value.trim() || '*',
    target_wim: document.getElementById('rule-wim').value.trim()
  };
  const res = await fetch('/rules/create', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    document.getElementById('rule-name').value = '';
    document.getElementById('rule-mfg').value = '';
    document.getElementById('rule-wim').value = '';
    showToast('Added BIOS Auto-Image Rule!');
    refreshOverview();
  }
}

async function deleteRule(id) {
  const res = await fetch('/rules/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id })
  });
  await res.json();
  showToast('Deleted rule');
  refreshOverview();
}

async function queueDeviceBoot(mac, nextBootAction) {
  let assignedOSWim = '';
  if (nextBootAction.startsWith('os:')) {
    assignedOSWim = nextBootAction.substring(3);
  }
  const res = await fetch('/devices/queue', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ mac, next_boot_action: nextBootAction, assigned_os_wim: assignedOSWim })
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    showToast(`Queued next boot for ${mac} -> ${nextBootAction}`);
    refreshOverview();
  }
}

async function createNode(event) {
  event.preventDefault();
  const payload = {
    name: document.getElementById('node-name').value.trim(),
    url: document.getElementById('node-url').value.trim(),
    api_key: document.getElementById('node-key').value.trim()
  };
  const res = await fetch('/nodes/create', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Error: ${data.error}`);
  } else {
    document.getElementById('node-name').value = '';
    document.getElementById('node-url').value = '';
    document.getElementById('node-key').value = '';
    showToast('Registered warehouse node!');
    refreshOverview();
  }
}

async function deleteNode(id) {
  if (!confirm('Remove this warehouse node?')) return;
  await fetch('/nodes/delete', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ id })
  });
  showToast('Removed node');
  refreshOverview();
}

async function syncNodesNow() {
  const res = await fetch('/nodes/sync-now', { method: 'POST' });
  const data = await res.json();
  if (data.error) {
    showToast(`Sync error: ${data.error}`);
  } else {
    showToast(data.message || 'Started warehouse sync job!');
    refreshOverview();
  }
}

async function pullNodesDBNow() {
  showToast('Pulling device records & logs from warehouse nodes...');
  try {
    const res = await fetch('/nodes/pull-db', { method: 'POST' });
    const data = await res.json();
    if (data.error) {
      showToast(`DB Sync error: ${data.error}`);
    } else {
      showToast(data.message || 'Pulled remote warehouse database records!');
      refreshOverview();
    }
  } catch (err) {
    showToast(`DB Sync failed: ${err.message}`);
  }
}

async function runSystemUpdate() {
  if (!confirm('Run ./update.sh to pull latest repo changes, build/update, and restart services?')) return;
  const outBox = document.getElementById('update-script-output');
  if (outBox) {
    outBox.textContent = 'Running ./update.sh ...';
    outBox.classList.remove('hidden');
  }
  showToast('Running ./update.sh ...');
  try {
    const res = await fetch('/system/update', { method: 'POST' });
    const data = await res.json();
    if (data.error) {
      if (outBox) outBox.textContent = data.output || data.error;
      showToast(`Update failed: ${data.error}`);
    } else {
      if (outBox) outBox.textContent = data.output || data.message;
      showToast(data.message || 'Application updated!');
      setTimeout(refreshOverview, 2500);
    }
  } catch (err) {
    showToast(`Update request finished/restarting: ${err.message}`);
    setTimeout(refreshOverview, 3000);
  }
}

async function saveSyncPolicy(event) {
  event.preventDefault();
  const payload = {
    node_name: document.getElementById('cfg-node-name').value.trim(),
    server_ip: document.getElementById('cfg-server-ip').value.trim(),
    default_wim: document.getElementById('cfg-default-wim').value.trim(),
    tftp_port: parseInt(document.getElementById('cfg-tftp-port').value, 10),
    http_port: parseInt(document.getElementById('cfg-http-port').value, 10),
    ruby_web_port: parseInt(document.getElementById('cfg-ruby-port').value, 10),
    menu_timeout_ms: parseInt(document.getElementById('cfg-timeout').value, 10),
    linux_kernel_args: document.getElementById('cfg-linux-args').value.trim(),
    auto_launch_winpe_installer: document.getElementById('cfg-autolaunch').checked,
    sync_schedule: document.getElementById('cfg-sync-schedule').value.trim(),
    sync_max_mbps: parseInt(document.getElementById('cfg-sync-mbps').value, 10) || 0,
    sync_rules_with_images: document.getElementById('cfg-sync-rules').checked
  };

  const res = await fetch('/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Failed to save sync policy: ${data.error}`);
  } else {
    showToast('Saved Multi-Site Sync Policy!');
    refreshOverview();
  }
}

async function saveSettings(event) {
  event.preventDefault();
  const payload = {
    node_name: document.getElementById('cfg-node-name').value.trim(),
    server_ip: document.getElementById('cfg-server-ip').value.trim(),
    default_wim: document.getElementById('cfg-default-wim').value.trim(),
    tftp_port: parseInt(document.getElementById('cfg-tftp-port').value, 10),
    http_port: parseInt(document.getElementById('cfg-http-port').value, 10),
    ruby_web_port: parseInt(document.getElementById('cfg-ruby-port').value, 10),
    menu_timeout_ms: parseInt(document.getElementById('cfg-timeout').value, 10),
    linux_kernel_args: document.getElementById('cfg-linux-args').value.trim(),
    auto_launch_winpe_installer: document.getElementById('cfg-autolaunch').checked,
    sync_schedule: document.getElementById('cfg-sync-schedule') ? document.getElementById('cfg-sync-schedule').value.trim() : '02:00',
    sync_max_mbps: document.getElementById('cfg-sync-mbps') ? (parseInt(document.getElementById('cfg-sync-mbps').value, 10) || 0) : 0,
    sync_rules_with_images: document.getElementById('cfg-sync-rules') ? document.getElementById('cfg-sync-rules').checked : true
  };

  const res = await fetch('/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const data = await res.json();
  if (data.error) {
    showToast(`Failed to save settings: ${data.error}`);
  } else {
    showToast('Saved settings and updated tftp_files/boot.ipxe!');
    refreshOverview();
  }
}

async function simulateBoot() {
  const res = await fetch('/simulate-boot', { method: 'POST' });
  const data = await res.json();
  showToast(data.message || 'Simulation started!');
  switchTab('dashboard');
  refreshOverview();
}

function escapeHtml(str) {
  return String(str ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function escapeJs(str) {
  return String(str ?? '').replace(/\\/g, '\\\\').replace(/'/g, "\\'");
}

refreshOverview();
setInterval(refreshOverview, 1500);
