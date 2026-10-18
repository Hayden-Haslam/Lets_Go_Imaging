let settingsLoadedOnce = false;

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

    renderStats(data);
    renderTransfers(data.transfers || []);
    renderDevices(data.devices || []);
    renderImages(data);
    renderLogs(data.logs || []);
    renderSettings(data);
  } catch (err) {
    console.error('Failed to poll overview:', err);
  }
}

function renderStats(data) {
  const cfg = data.config || {};
  document.getElementById('stat-server-ip').textContent = cfg.server_ip || '--';
  document.getElementById('stat-ports').textContent = `TFTP :${cfg.tftp_port || 69} | HTTP :${cfg.http_port || 80} | Web :${cfg.ruby_web_port || 4567}`;

  const winReady = !!data.winpe_ready;
  const winEl = document.getElementById('stat-winpe-status');
  winEl.textContent = winReady ? 'Ready to Boot' : 'Needs WIM File';
  winEl.style.color = winReady ? '#34d399' : '#fbbf24';
  document.getElementById('stat-default-wim').textContent = `Default WIM: ${cfg.default_wim || 'boot.wim'}`;

  const linuxReady = !!data.linux_ready;
  const linuxEl = document.getElementById('stat-linux-status');
  linuxEl.textContent = linuxReady ? 'Ready to Boot' : 'Placeholder Mode';
  linuxEl.style.color = linuxReady ? '#34d399' : '#60a5fa';

  document.getElementById('stat-device-count').textContent = (data.devices || []).length;
}

function renderTransfers(transfers) {
  const container = document.getElementById('transfers-container');
  if (!transfers.length) {
    container.innerHTML = '<p class="empty-state">No active image downloads right now. Boot a PXE client or click "⚡ Simulate PXE Client Boot" above.</p>';
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

function renderDevices(devices) {
  const dashTbody = document.getElementById('dashboard-devices-tbody');
  const fullTbody = document.getElementById('devices-tbody');

  if (!devices.length) {
    dashTbody.innerHTML = '<tr><td colspan="7" class="empty-cell">No devices discovered yet.</td></tr>';
    fullTbody.innerHTML = '<tr><td colspan="9" class="empty-cell">No PXE devices have checked in yet.</td></tr>';
    return;
  }

  dashTbody.innerHTML = devices.slice(0, 6).map(d => `
    <tr>
      <td><code>${escapeHtml(d.mac)}</code></td>
      <td>${escapeHtml(d.ip)}</td>
      <td><strong>${escapeHtml(d.manufacturer)}</strong> ${escapeHtml(d.model)}</td>
      <td><code>${escapeHtml(d.serial)}</code></td>
      <td>${escapeHtml(d.current_image || '-')}</td>
      <td><span class="badge badge-info">${escapeHtml(d.current_status)}</span></td>
      <td class="muted">${escapeHtml(d.last_seen)}</td>
    </tr>
  `).join('');

  fullTbody.innerHTML = devices.map(d => `
    <tr>
      <td><code>${escapeHtml(d.mac)}</code></td>
      <td>${escapeHtml(d.ip)}</td>
      <td>${escapeHtml(d.manufacturer)}</td>
      <td><strong>${escapeHtml(d.model)}</strong></td>
      <td><code>${escapeHtml(d.serial)}</code></td>
      <td>${escapeHtml(d.arch)}</td>
      <td>${d.boot_count}</td>
      <td>
        <span class="badge badge-info">${escapeHtml(d.current_status)}</span>
        ${d.progress_pct > 0 ? `<small class="muted">(${d.progress_pct}%)</small>` : ''}
      </td>
      <td class="muted">${escapeHtml(d.last_seen)}</td>
    </tr>
  `).join('');
}

function renderImages(data) {
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
          ? '<span class="badge badge-ok">★ Active Default WIM</span>'
          : '<span class="badge badge-info">Windows WIM</span>';
      }

      let actions = '';
      if (f.is_wim && !f.is_default_wim) {
        actions += `<button class="btn btn-sm btn-secondary" onclick="setDefaultWim('${escapeJs(f.name)}')">Set Default</button> `;
      }
      if (!f.protected) {
        actions += `<button class="btn btn-sm btn-danger" onclick="deleteImage('windows', '${escapeJs(f.name)}')">Remove</button>`;
      } else {
        actions += '<span class="muted">Protected</span>';
      }

      return `
        <tr>
          <td><strong>${escapeHtml(f.name)}</strong></td>
          <td>${escapeHtml(f.size_human)}</td>
          <td class="muted">${escapeHtml(f.modified_at)}</td>
          <td>${roleBadge}</td>
          <td>${actions}</td>
        </tr>
      `;
    }).join('');
  }

  const linuxFiles = data.linux_files || [];
  const linuxTbody = document.getElementById('linux-files-tbody');
  if (!linuxFiles.length) {
    linuxTbody.innerHTML = '<tr><td colspan="4" class="empty-cell">No Linux kernel/initrd uploaded yet (Placeholder active in boot menu).</td></tr>';
  } else {
    linuxTbody.innerHTML = linuxFiles.map(f => `
      <tr>
        <td><strong>${escapeHtml(f.name)}</strong></td>
        <td>${escapeHtml(f.size_human)}</td>
        <td class="muted">${escapeHtml(f.modified_at)}</td>
        <td><button class="btn btn-sm btn-danger" onclick="deleteImage('linux', '${escapeJs(f.name)}')">Remove</button></td>
      </tr>
    `).join('');
  }
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

function renderSettings(data) {
  const cfg = data.config || {};
  const ifaces = data.interfaces || [];

  const ifaceContainer = document.getElementById('interfaces-list');
  if (ifaces.length) {
    ifaceContainer.innerHTML = ifaces.map(i => `
      <button type="button" class="iface-pill" onclick="document.getElementById('cfg-server-ip').value='${escapeJs(i.ip)}'">
        ${escapeHtml(i.name)}: <strong>${escapeHtml(i.ip)}</strong> (${escapeHtml(i.mac)})
      </button>
    `).join('');
  } else {
    ifaceContainer.innerHTML = '<span class="muted">No non-loopback IPv4 interfaces detected</span>';
  }

  if (data.systemd_go) {
    document.getElementById('systemd-go-preview').textContent = data.systemd_go;
  }
  if (data.systemd_ruby) {
    document.getElementById('systemd-ruby-preview').textContent = data.systemd_ruby;
  }

  if (!settingsLoadedOnce) {
    document.getElementById('cfg-server-ip').value = cfg.server_ip || '';
    document.getElementById('cfg-default-wim').value = cfg.default_wim || 'boot.wim';
    document.getElementById('cfg-tftp-port').value = cfg.tftp_port || 69;
    document.getElementById('cfg-http-port').value = cfg.http_port || 80;
    document.getElementById('cfg-ruby-port').value = cfg.ruby_web_port || 4567;
    document.getElementById('cfg-timeout').value = cfg.menu_timeout_ms || 10000;
    document.getElementById('cfg-linux-args').value = cfg.linux_kernel_args || '';
    document.getElementById('cfg-autolaunch').checked = !!cfg.auto_launch_winpe_installer;
    settingsLoadedOnce = true;
  }
}

async function uploadImage(event, category) {
  event.preventDefault();
  const inputId = category === 'linux' ? 'linux-file-input' : 'win-file-input';
  const progressId = category === 'linux' ? 'linux-upload-progress' : 'win-upload-progress';
  const fileInput = document.getElementById(inputId);
  const progressEl = document.getElementById(progressId);

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
  if (!confirm(`Remove ${filename} from ${category} images?`)) return;
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
    showToast(`Active Windows WIM set to ${filename}`);
    refreshOverview();
  }
}

async function saveSettings(event) {
  event.preventDefault();
  const payload = {
    server_ip: document.getElementById('cfg-server-ip').value.trim(),
    default_wim: document.getElementById('cfg-default-wim').value.trim(),
    tftp_port: parseInt(document.getElementById('cfg-tftp-port').value, 10),
    http_port: parseInt(document.getElementById('cfg-http-port').value, 10),
    ruby_web_port: parseInt(document.getElementById('cfg-ruby-port').value, 10),
    menu_timeout_ms: parseInt(document.getElementById('cfg-timeout').value, 10),
    linux_kernel_args: document.getElementById('cfg-linux-args').value.trim(),
    auto_launch_winpe_installer: document.getElementById('cfg-autolaunch').checked
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
