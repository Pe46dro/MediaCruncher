// ==========================================================================
// MediaCruncher SPA Client Application
// ==========================================================================

let currentConfig = null;
let currentQueueState = "all";
let autoRefreshTimer = null;
let isAutoRefresh = true;
let editingRuleIndex = -1;
let editingPresetIndex = -1;
let selectedPriorityJobId = null;

document.addEventListener("DOMContentLoaded", () => {
  initTabs();
  initModals();
  initControls();
  
  // Initial data loading
  fetchStatus();
  fetchQueue();
  fetchConfig();
  fetchHardware();

  // Start 3s polling loop
  startPolling();
});

// ==========================================================================
// Tab Navigation
// ==========================================================================
function initTabs() {
  const tabButtons = document.querySelectorAll(".tab-btn");
  const tabPanes = document.querySelectorAll(".tab-pane");

  tabButtons.forEach(btn => {
    btn.addEventListener("click", () => {
      tabButtons.forEach(b => b.classList.remove("active"));
      tabPanes.forEach(p => p.classList.remove("active"));

      btn.classList.add("active");
      const target = btn.getAttribute("data-tab");
      const pane = document.getElementById(target);
      if (pane) pane.classList.add("active");

      if (target === "tab-queue") fetchQueue();
      if (target === "tab-rules" || target === "tab-presets" || target === "tab-settings") fetchConfig();
    });
  });
}

// ==========================================================================
// Modals
// ==========================================================================
function initModals() {
  document.querySelectorAll(".modal-close, .modal-backdrop").forEach(el => {
    el.addEventListener("click", (e) => {
      if (e.target === el) {
        document.querySelectorAll(".modal-backdrop").forEach(m => m.classList.remove("open"));
      }
    });
  });
}

function openModal(id) {
  const m = document.getElementById(id);
  if (m) m.classList.add("open");
}

function closeModal(id) {
  const m = document.getElementById(id);
  if (m) m.classList.remove("open");
}

// ==========================================================================
// Controls & Actions
// ==========================================================================
function initControls() {
  // Auto-refresh toggle
  const refreshBtn = document.getElementById("btn-toggle-refresh");
  refreshBtn.addEventListener("click", () => {
    isAutoRefresh = !isAutoRefresh;
    refreshBtn.textContent = `Auto-Refresh: ${isAutoRefresh ? "ON" : "OFF"}`;
    if (isAutoRefresh) {
      startPolling();
      showToast("Auto-refresh enabled (3s)", "success");
    } else {
      stopPolling();
      showToast("Auto-refresh paused", "success");
    }
  });

  // Trigger scan button
  document.getElementById("btn-trigger-scan").addEventListener("click", async () => {
    try {
      const res = await fetch("/api/scan", { method: "POST" });
      if (res.ok) {
        showToast("Filesystem scan started in background", "success");
        setTimeout(fetchStatus, 500);
      } else {
        showToast("Failed to trigger scan", "error");
      }
    } catch (e) {
      showToast("Network error triggering scan", "error");
    }
  });

  // Queue filter pills
  document.querySelectorAll("#queue-filters .filter-pill").forEach(pill => {
    pill.addEventListener("click", () => {
      document.querySelectorAll("#queue-filters .filter-pill").forEach(p => p.classList.remove("active"));
      pill.classList.add("active");
      currentQueueState = pill.getAttribute("data-state");
      fetchQueue();
    });
  });

  // Queue search input
  let searchTimeout = null;
  document.getElementById("queue-search").addEventListener("input", (e) => {
    clearTimeout(searchTimeout);
    searchTimeout = setTimeout(fetchQueue, 300);
  });

  // Refresh queue button
  document.getElementById("btn-refresh-queue").addEventListener("click", fetchQueue);

  // Add rule button
  document.getElementById("btn-add-rule").addEventListener("click", () => {
    editingRuleIndex = -1;
    document.getElementById("modal-rule-title").textContent = "Add New Evaluation Rule";
    document.getElementById("rule-name").value = "";
    document.getElementById("rule-priority").value = "50";
    document.getElementById("rule-action").value = "transcode";
    document.getElementById("rule-preset").value = "balanced-hevc";
    document.getElementById("rule-cond-codec").value = "";
    document.getElementById("rule-cond-res").value = "";
    openModal("modal-rule");
  });

  // Save rule from modal
  document.getElementById("btn-save-rule-dialog").addEventListener("click", saveRuleFromModal);

  // Add preset button
  document.getElementById("btn-add-preset").addEventListener("click", () => {
    editingPresetIndex = -1;
    document.getElementById("modal-preset-title").textContent = "Add Encoding Preset";
    document.getElementById("preset-name").value = "";
    document.getElementById("preset-codec").value = "hevc";
    document.getElementById("preset-crf").value = "22";
    document.getElementById("preset-speed").value = "medium";
    document.getElementById("preset-audio-codec").value = "copy";
    document.getElementById("preset-extra").value = "";
    openModal("modal-preset");
  });

  // Save preset from modal
  document.getElementById("btn-save-preset-dialog").addEventListener("click", savePresetFromModal);

  // Save preservation
  document.getElementById("btn-save-preservation").addEventListener("click", savePreservation);

  // Save all settings
  document.getElementById("btn-save-all-config").addEventListener("click", saveAllSettings);

  // Save priority from modal
  const savePriorityBtn = document.getElementById("btn-save-priority-dialog");
  if (savePriorityBtn) {
    savePriorityBtn.addEventListener("click", savePriority);
  }

  // Preset buttons in priority modal
  document.querySelectorAll(".priority-preset-btn").forEach(btn => {
    btn.addEventListener("click", () => {
      const val = btn.getAttribute("data-priority");
      const input = document.getElementById("priority-custom-input");
      if (input) input.value = val;
      document.querySelectorAll(".priority-preset-btn").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
    });
  });

  // Custom priority input synchronizer
  const priorityInput = document.getElementById("priority-custom-input");
  if (priorityInput) {
    priorityInput.addEventListener("input", (e) => {
      const val = e.target.value.trim();
      document.querySelectorAll(".priority-preset-btn").forEach(b => {
        if (b.getAttribute("data-priority") === val) {
          b.classList.add("active");
        } else {
          b.classList.remove("active");
        }
      });
    });
  }
}

function startPolling() {
  stopPolling();
  autoRefreshTimer = setInterval(() => {
    if (isAutoRefresh) {
      fetchStatus();
      const activeTab = document.querySelector(".tab-pane.active");
      if (activeTab && activeTab.id === "tab-queue") {
        fetchQueue(true); // silent refresh
      }
    }
  }, 3000);
}

function stopPolling() {
  if (autoRefreshTimer) {
    clearInterval(autoRefreshTimer);
    autoRefreshTimer = null;
  }
}

// ==========================================================================
// API Calls: Status & Telemetry
// ==========================================================================
async function fetchStatus() {
  try {
    const res = await fetch("/api/status");
    if (!res.ok) return;
    const data = await res.json();

    document.getElementById("stat-scanned").textContent = data.files_scanned.toLocaleString();
    document.getElementById("stat-deduped").textContent = data.deduplicated_files.toLocaleString();
    document.getElementById("stat-completed").textContent = data.jobs_completed.toLocaleString();
    document.getElementById("stat-failed").textContent = data.jobs_failed.toLocaleString();
    document.getElementById("stat-saved").textContent = formatBytes(data.bytes_saved);
    document.getElementById("stat-vmaf").textContent = data.average_vmaf > 0 ? data.average_vmaf.toFixed(2) : "--";

    document.getElementById("stat-gpu-active").textContent = data.active_gpu_workers;
    document.getElementById("stat-cpu-active").textContent = data.active_cpu_workers;
    if (document.getElementById("stat-gpu-limit")) {
      document.getElementById("stat-gpu-limit").textContent = `/${data.gpu_limit || 2}`;
    }
    if (document.getElementById("stat-cpu-limit")) {
      document.getElementById("stat-cpu-limit").textContent = `/${data.cpu_limit || 4}`;
    }

    document.getElementById("queue-badge-count").textContent = data.queue.pending + data.queue.leased;
    document.getElementById("queue-breakdown-total").textContent = `${data.queue.total} total items`;

    // Render health grid
    renderHealthGrid(data.health);

    // Render Donut breakdown
    renderDonutChart(data.queue);

    // Render savings chart
    renderSavingsChart(data.recent_savings);

    // Render audit feed
    renderAuditFeed(data.recent_audit);
  } catch (e) {
    console.error("Failed to fetch status:", e);
  }
}

function renderHealthGrid(health) {
  const grid = document.getElementById("health-grid");
  if (!grid || !health) return;
  grid.innerHTML = "";

  for (const [module, status] of Object.entries(health)) {
    const item = document.createElement("div");
    item.className = "health-item";
    item.innerHTML = `
      <span class="health-name">${escapeHtml(module)}</span>
      <span class="badge badge-${status.toLowerCase()}">${status}</span>
    `;
    grid.appendChild(item);
  }
}

function renderDonutChart(queue) {
  const svg = document.getElementById("svg-donut");
  if (!svg) return;

  const total = (queue.completed || 0) + (queue.pending || 0) + (queue.leased || 0) + (queue.failed || 0) + (queue.skipped || 0);
  if (total === 0) {
    svg.innerHTML = `
      <circle cx="90" cy="90" r="70" fill="none" stroke="rgba(255,255,255,0.06)" stroke-width="22" />
      <text x="90" y="95" text-anchor="middle" fill="#64748b" font-size="12" font-family="sans-serif">Empty Queue</text>
    `;
    return;
  }

  const slices = [
    { label: "Completed", count: queue.completed || 0, color: "#10b981" },
    { label: "Transcoding", count: queue.leased || 0, color: "#f59e0b" },
    { label: "Pending", count: queue.pending || 0, color: "#6366f1" },
    { label: "Skipped", count: queue.skipped || 0, color: "#94a3b8" },
    { label: "Failed", count: queue.failed || 0, color: "#f43f5e" }
  ];

  let currentAngle = -90;
  let paths = "";
  const r = 70;
  const cx = 90;
  const cy = 90;

  slices.forEach(slice => {
    if (slice.count === 0) return;
    const angle = (slice.count / total) * 360;
    const startAngle = currentAngle;
    const endAngle = currentAngle + angle;
    currentAngle = endAngle;

    const x1 = cx + r * Math.cos(Math.PI * startAngle / 180);
    const y1 = cy + r * Math.sin(Math.PI * startAngle / 180);
    const x2 = cx + r * Math.cos(Math.PI * endAngle / 180);
    const y2 = cy + r * Math.sin(Math.PI * endAngle / 180);
    const largeArc = angle > 180 ? 1 : 0;

    paths += `<path d="M ${cx} ${cy} L ${x1} ${y1} A ${r} ${r} 0 ${largeArc} 1 ${x2} ${y2} Z" fill="${slice.color}" opacity="0.85">
      <title>${slice.label}: ${slice.count}</title>
    </path>`;
  });

  // Inner cutout for donut
  paths += `<circle cx="${cx}" cy="${cy}" r="45" fill="var(--bg-surface)" />`;
  paths += `<text x="${cx}" y="${cy+4}" text-anchor="middle" fill="#ffffff" font-size="14" font-weight="700">${total}</text>`;
  svg.innerHTML = paths;
}

function renderSavingsChart(savings) {
  const svg = document.getElementById("svg-savings");
  const listContainer = document.getElementById("savings-list");
  if (!svg) return;
  svg.innerHTML = "";
  if (listContainer) listContainer.innerHTML = "";

  if (!savings || savings.length === 0) {
    svg.innerHTML = `<text x="50%" y="50%" text-anchor="middle" fill="#64748b" font-size="13">No recent completed transcodes</text>`;
    return;
  }

  // Calculate SVG layout
  const count = savings.length;
  const maxVal = Math.max(...savings.map(s => s.saved_bytes || 0), 1024);
  const svgWidth = svg.clientWidth || svg.parentElement.clientWidth || 420;
  const svgHeight = 150;
  const chartTop = 26;
  const chartBottom = svgHeight - 26;
  const usableHeight = chartBottom - chartTop;

  // Restrict barWidth so a single bar never balloons across the whole card!
  const maxBarWidth = 48;
  const minBarWidth = 28;
  const barWidth = Math.min(maxBarWidth, Math.max(minBarWidth, Math.floor((svgWidth - 60) / count) - 14));
  const spacing = 14;
  const totalBarsWidth = count * barWidth + (count - 1) * spacing;
  const startX = Math.max(25, Math.floor((svgWidth - totalBarsWidth) / 2));

  let elements = `
    <!-- Baseline axis -->
    <line x1="15" y1="${chartBottom}" x2="${svgWidth - 15}" y2="${chartBottom}" stroke="rgba(255,255,255,0.1)" stroke-width="1" />
  `;

  savings.forEach((s, idx) => {
    const rawSaved = s.saved_bytes || 0;
    const barHeight = Math.max(10, Math.round((rawSaved / maxVal) * usableHeight));
    const x = startX + idx * (barWidth + spacing);
    const y = chartBottom - barHeight;

    const savedFormatted = formatBytes(rawSaved);
    const labelX = x + barWidth / 2;

    elements += `
      <!-- Bar column -->
      <rect x="${x}" y="${y}" width="${barWidth}" height="${barHeight}" rx="4" fill="url(#savingsGradient)" opacity="0.92">
        <title>Job #${s.queue_id}: Saved ${savedFormatted}</title>
      </rect>
      <!-- Top Value Label -->
      <text x="${labelX}" y="${Math.max(14, y - 6)}" text-anchor="middle" fill="#34d399" font-size="11" font-weight="600">
        +${savedFormatted}
      </text>
      <!-- Bottom Job Label -->
      <text x="${labelX}" y="${chartBottom + 16}" text-anchor="middle" fill="#94a3b8" font-size="11">
        #${s.queue_id}
      </text>
    `;
  });

  svg.innerHTML = `
    <defs>
      <linearGradient id="savingsGradient" x1="0" y1="0" x2="0" y2="1">
        <stop offset="0%" stop-color="#10b981"/>
        <stop offset="100%" stop-color="#06b6d4"/>
      </linearGradient>
    </defs>
    ${elements}
  `;

  // Render detailed rows list beneath the chart
  if (listContainer) {
    savings.forEach(s => {
      const row = document.createElement("div");
      row.className = "saving-row";
      const pct = s.orig_size > 0 ? Math.round((s.saved_bytes / s.orig_size) * 100) : 0;
      const pctHtml = pct > 0 ? `<span class="saving-pill">-${pct}%</span>` : "";
      const origStr = s.orig_size > 0 ? formatBytes(s.orig_size) : "";
      const newStr = s.new_size > 0 ? formatBytes(s.new_size) : "";
      const sizeComparison = origStr && newStr ? `<span style="color:var(--text-secondary); font-size:0.75rem;">${origStr} &rarr; ${newStr}</span>` : "";

      row.innerHTML = `
        <div style="display:flex; align-items:center; gap:0.5rem; min-width:0;">
          <span style="font-weight:600; color:var(--text-primary);">Job #${s.queue_id}</span>
          ${sizeComparison}
        </div>
        <div style="display:flex; align-items:center; gap:0.5rem;">
          <span style="color:var(--accent-emerald); font-weight:600;">+${formatBytes(s.saved_bytes)}</span>
          ${pctHtml}
        </div>
      `;
      listContainer.appendChild(row);
    });
  }
}

function renderAuditFeed(logs) {
  const container = document.getElementById("audit-feed");
  if (!container) return;
  container.innerHTML = "";

  if (!logs || logs.length === 0) {
    container.innerHTML = `<div style="text-align:center; color:var(--text-muted); font-size:0.8rem; padding:1.5rem;">No recent audit events</div>`;
    return;
  }

  logs.forEach(log => {
    let payload = {};
    try {
      payload = JSON.parse(log.payload_json || "{}");
    } catch(e) {
      payload = { raw: log.payload_json };
    }

    const item = document.createElement("div");
    item.className = "audit-item";

    const time = new Date(log.timestamp).toLocaleTimeString();
    let badgeClass = "badge-pending";
    let eventTitle = log.event_type;
    let summaryHtml = "";
    let detailsHtml = "";

    switch (log.event_type) {
      case "job_completed":
        badgeClass = "badge-completed";
        eventTitle = "Completed";
        const saved = payload.saved_bytes ? formatBytes(payload.saved_bytes) : "0 B";
        const enc = payload.encoder || "transcoder";
        const dur = payload.duration ? `${payload.duration.toFixed(1)}s` : "";
        summaryHtml = `Job #${payload.queue_id || '?'}: Saved <strong style="color:var(--accent-emerald);">${saved}</strong> (${enc}${dur ? ', ' + dur : ''})`;
        break;

      case "job_failed":
        badgeClass = "badge-failed";
        eventTitle = "Failed";
        let errMsg = payload.error || "Transcode execution failed";
        // Extract the most informative first line
        const errLines = errMsg.split("\n").map(l => l.trim()).filter(l => l.length > 0);
        let shortErr = errLines[0] || errMsg;
        if (shortErr.length > 70) shortErr = shortErr.substring(0, 70) + "...";
        summaryHtml = `Job #${payload.queue_id || '?'}: <span style="color:#f87171;">${escapeHtml(shortErr)}</span>`;
        detailsHtml = `
          <details class="audit-details">
            <summary>View Diagnostic Log</summary>
            <pre class="audit-error-pre">${escapeHtml(errMsg)}</pre>
          </details>
        `;
        break;

      case "job_skipped_growth":
        badgeClass = "badge-skipped";
        eventTitle = "Skipped";
        summaryHtml = `Job #${payload.queue_id || '?'}: Size protection (${formatBytes(payload.orig_size)} &rarr; ${formatBytes(payload.new_size)})`;
        break;

      case "crash_recovery":
      case "startup_recovery":
        badgeClass = "badge-evaluating";
        eventTitle = "Recovery";
        summaryHtml = `Recovered ${payload.recovered_jobs || 0} orphaned job(s) for execution`;
        break;

      default:
        badgeClass = log.severity === 'error' ? 'badge-failed' : 'badge-completed';
        summaryHtml = escapeHtml(typeof payload === 'object' ? JSON.stringify(payload) : String(payload));
        break;
    }

    item.innerHTML = `
      <div class="audit-item-header">
        <div class="audit-item-title">
          <span class="badge ${badgeClass}">${eventTitle}</span>
          <span class="audit-item-summary">${summaryHtml}</span>
        </div>
        <span class="audit-item-time">${time}</span>
      </div>
      ${detailsHtml}
    `;
    container.appendChild(item);
  });
}

// ==========================================================================
// API Calls: Queue Management
// ==========================================================================
async function fetchQueue(silent = false) {
  try {
    const search = document.getElementById("queue-search") ? document.getElementById("queue-search").value.trim() : "";
    const url = `/api/queue?state=${encodeURIComponent(currentQueueState)}&search=${encodeURIComponent(search)}&limit=100`;
    const res = await fetch(url);
    if (!res.ok) return;
    const data = await res.json();
    renderQueueTable(data.entries, data.job_stats || {}, data.active_progress || {});
  } catch (e) {
    if (!silent) console.error("Failed to fetch queue:", e);
  }
}

function renderQueueTable(entries, jobStats = {}, activeProgress = {}) {
  const tbody = document.getElementById("queue-table-body");
  if (!tbody) return;
  tbody.innerHTML = "";

  if (!entries || entries.length === 0) {
    tbody.innerHTML = `<tr><td colspan="8" style="text-align:center; padding:2rem; color:var(--text-muted);">No queue items found matching filter</td></tr>`;
    return;
  }

  entries.forEach(entry => {
    const tr = document.createElement("tr");
    const fileName = entry.file_path.split(/[\\/]/).pop();
    const scheduled = new Date(entry.scheduled_at).toLocaleTimeString();
    const stats = jobStats[entry.id] || null;

    let resultHTML = `<span style="color:var(--text-muted); font-size:0.75rem;">--</span>`;

    if (entry.state === "completed" && stats) {
      const savedStr = stats.saved_bytes > 0 ? `+${formatBytes(stats.saved_bytes)}` : "0 B";
      const pct = stats.orig_size > 0 ? Math.round((stats.saved_bytes / stats.orig_size) * 100) : 0;
      const vmafStr = stats.vmaf > 0 ? `VMAF ${stats.vmaf.toFixed(1)}` : "";
      resultHTML = `
        <div style="display:flex; flex-direction:column; gap:2px;">
          <div style="display:flex; align-items:center; gap:6px;">
            <strong style="color:var(--accent-emerald); font-size:0.82rem;">${savedStr}</strong>
            ${pct > 0 ? `<span class="saving-pill" style="font-size:0.68rem; padding:1px 5px;">-${pct}%</span>` : ""}
          </div>
          <div style="color:var(--text-secondary); font-size:0.72rem;">
            ${vmafStr ? `<span style="color:var(--accent-cyan); font-weight:500;">${vmafStr}</span>` : ""}
            ${stats.encoder ? `<span style="margin-left:4px; opacity:0.8;">(${stats.encoder})</span>` : ""}
          </div>
        </div>
      `;
    } else if (entry.state === "quality_failed" || (stats && stats.vmaf && stats.vmaf > 0 && entry.state !== "completed")) {
      const vmafScore = stats && stats.vmaf ? stats.vmaf.toFixed(1) : (entry.error_message.match(/VMAF\s+([\d\.]+)/)?.[1] || "Low");
      resultHTML = `
        <div style="display:flex; flex-direction:column; gap:2px;">
          <span style="color:var(--accent-rose); font-weight:600; font-size:0.82rem;">VMAF ${vmafScore}</span>
          <span style="color:var(--text-muted); font-size:0.7rem; max-width:200px; white-space:nowrap; overflow:hidden; text-overflow:ellipsis;" title="${escapeHtml(entry.error_message)}">Below threshold</span>
        </div>
      `;
    } else if (entry.state === "skipped") {
      resultHTML = `<span style="color:var(--text-muted); font-size:0.75rem;">${escapeHtml(entry.error_message || "Rule / Size protection")}</span>`;
    } else if (entry.state === "paused") {
      resultHTML = `<span style="color:#c084fc; font-size:0.75rem;">⏸ In pausa dall'utente</span>`;
    } else if (entry.state === "failed" || entry.state === "permanently_failed") {
      resultHTML = `<span style="color:var(--accent-rose); font-size:0.75rem; max-width:200px; display:inline-block; white-space:nowrap; overflow:hidden; text-overflow:ellipsis;" title="${escapeHtml(entry.error_message)}">${escapeHtml(entry.error_message || "Execution error")}</span>`;
    } else {
      // Active states: transcoding, evaluating, leased, processing
      const prog = activeProgress ? activeProgress[entry.id] : null;
      if (prog) {
        if (prog.phase === "evaluating") {
          resultHTML = `
            <div class="queue-progress-box">
              <div style="display:flex; align-items:center; gap:6px;">
                <span class="badge-eval-pill">Evaluating</span>
                <span style="font-size:0.72rem; color:var(--text-secondary); white-space:nowrap; overflow:hidden; text-overflow:ellipsis;" title="${escapeHtml(prog.phase_detail || '')}">${escapeHtml(prog.phase_detail || 'Probing streams...')}</span>
              </div>
            </div>
          `;
        } else if (prog.phase === "verifying_vmaf") {
          resultHTML = `
            <div class="queue-progress-box">
              <div class="queue-telemetry-primary">
                <span class="badge-vmaf-pill">VMAF Check</span>
                <span style="color:var(--accent-cyan); font-weight:600; font-size:0.75rem;">${prog.percentage.toFixed(1)}%</span>
              </div>
              <div class="queue-progress-bar-wrap">
                <div class="queue-progress-bar-fill vmaf ${prog.is_stuck ? 'stuck' : ''}" style="width:${Math.min(100, Math.max(5, prog.percentage))}%"></div>
              </div>
              <div class="queue-telemetry-sub" title="${escapeHtml(prog.phase_detail || '')}">
                <span>${escapeHtml(prog.phase_detail || 'Quality scoring...')}</span>
              </div>
              ${prog.is_stuck ? `<div class="badge-stuck" title="No telemetry for ${prog.stuck_duration_sec}s">⚠️ STUCK (${prog.stuck_duration_sec}s)</div>` : ''}
            </div>
          `;
        } else if (prog.phase === "promoting") {
          resultHTML = `
            <div class="queue-progress-box">
              <span style="color:var(--accent-emerald); font-size:0.75rem; font-weight:600;">📦 Finalizing file...</span>
            </div>
          `;
        } else {
          // Transcoding phase
          const etaStr = prog.eta_seconds > 0 ? formatETA(prog.eta_seconds) : '--';
          const speedStr = prog.speed > 0 ? `${prog.speed.toFixed(2)}x` : '';
          const fpsStr = prog.fps > 0 ? `${Math.round(prog.fps)} fps` : '';
          const subInfo = [speedStr, fpsStr, prog.bitrate].filter(Boolean).join(" • ");

          resultHTML = `
            <div class="queue-progress-box">
              <div class="queue-telemetry-primary">
                <span style="color:var(--accent-amber); font-weight:700;">${prog.percentage.toFixed(1)}%</span>
                <span style="color:var(--text-secondary); font-size:0.72rem;">ETA: <strong style="color:var(--text-primary);">${etaStr}</strong></span>
              </div>
              <div class="queue-progress-bar-wrap">
                <div class="queue-progress-bar-fill ${prog.is_stuck ? 'stuck' : ''}" style="width:${Math.min(100, Math.max(3, prog.percentage))}%"></div>
              </div>
              <div class="queue-telemetry-sub" title="${escapeHtml(prog.phase_detail || '')}">
                <span>${subInfo || escapeHtml(prog.phase_detail || 'Encoding...')}</span>
              </div>
              ${prog.is_stuck ? `<div class="badge-stuck" title="No frames received for ${prog.stuck_duration_sec}s">⚠️ STUCK (${prog.stuck_duration_sec}s)</div>` : ''}
            </div>
          `;
        }
      } else {
        const initText = entry.state === "leased" ? "In attesa slot worker..." : "Inizializzazione...";
        resultHTML = `
          <span class="badge-init-pill" title="Job preso in carico dal worker. In attesa di avvio processo.">
            <span style="display:inline-block; width:6px; height:6px; border-radius:50%; background:#fbbf24;"></span>
            ${initText}
          </span>
        `;
      }
    }

    const isPausable = (entry.state === 'pending' || entry.state === 'leased' || entry.state === 'transcoding' || entry.state === 'evaluating' || entry.state === 'processing');
    const isResumable = (entry.state === 'paused' || entry.state === 'skipped' || entry.state === 'failed' || entry.state === 'quality_failed' || entry.state === 'permanently_failed');
    let badgeText = entry.state;
    if (entry.state === "leased") badgeText = "reserved";
    else if (entry.state === "paused") badgeText = "paused";

    tr.innerHTML = `
      <td style="font-weight:600;">#${entry.id}</td>
      <td class="file-cell" title="${escapeHtml(entry.file_path)}">${escapeHtml(fileName)}</td>
      <td><span class="badge badge-${entry.state}" title="${entry.state === 'leased' ? 'Prenotato dal worker per esecuzione imminente' : entry.state}">${badgeText}</span></td>
      <td>${resultHTML}</td>
      <td>
        <button class="priority-pill" onclick="openPriorityModal(${entry.id}, ${entry.priority})" title="Modifica priorità di elaborazione">
          ${entry.priority} <span style="font-size:0.7rem; opacity:0.75;">✎</span>
        </button>
      </td>
      <td>${entry.retry_count}</td>
      <td>${scheduled}</td>
      <td>
        <div style="display:flex; gap:0.35rem; align-items:center;">
          <button class="btn btn-secondary btn-sm" onclick="viewJobDetails(${entry.id})">Details</button>
          <button class="btn btn-secondary btn-sm" onclick="openPriorityModal(${entry.id}, ${entry.priority})" title="Imposta priorità di elaborazione">Priorità</button>
          ${isPausable ? 
            `<button class="btn btn-secondary btn-sm" style="color:#fbbf24; border-color:rgba(245,158,11,0.4);" onclick="pauseJob(${entry.id})" title="Metti in pausa temporanea (riprendibile)">Pause</button>
             <button class="btn btn-secondary btn-sm" style="color:#f87171; border-color:rgba(244,63,94,0.35);" onclick="ignoreJob(${entry.id})" title="Escludi (lo scanner automatico non lo reinserirà)">Escludi</button>` : ''}
          ${isResumable ? 
            `<button class="btn btn-primary btn-sm" onclick="requeueJob(${entry.id})" title="Ripristina e avvia transcodifica">Resume</button>` : ''}
          <button class="btn btn-danger btn-sm" onclick="deleteJob(${entry.id})" title="Elimina definitivamente dal database (attenzione: se il file è ancora su disco verrà rischedulato)" style="padding: 0.25rem 0.45rem; opacity: 0.65;">✕</button>
        </div>
      </td>
    `;
    tbody.appendChild(tr);
  });
}

async function viewJobDetails(id) {
  openModal("modal-job");
  const content = document.getElementById("modal-job-content");
  content.innerHTML = `<div style="text-align:center; padding:2rem;">Loading metadata and decision plan...</div>`;

  try {
    const res = await fetch(`/api/queue/${id}`);
    if (!res.ok) {
      content.innerHTML = `<div style="color:var(--accent-rose);">Failed to load job details.</div>`;
      return;
    }
    const data = await res.json();

    document.getElementById("modal-job-title").textContent = `Job #${id}: ${data.entry.file_path.split(/[\\/]/).pop()}`;

    let streamPlanHTML = "";
    if (data.metadata && data.metadata.stream_map_json) {
      try {
        const plan = JSON.parse(data.metadata.stream_map_json);
        streamPlanHTML = `
          <div style="margin-top:1rem;">
            <h4 style="font-size:0.85rem; font-weight:600; margin-bottom:0.4rem;">Synthesized Stream Plan:</h4>
            <p style="font-size:0.8rem; color:var(--text-secondary); margin-bottom:0.4rem;">${escapeHtml(plan.summary || "")}</p>
            <pre>Map Args: ${JSON.stringify(plan.map_args, null, 2)}\nAudio Args: ${JSON.stringify(plan.audio_codec_args, null, 2)}</pre>
          </div>
        `;
      } catch (err) {}
    }

    let liveTelemetryHTML = "";
    if (data.progress) {
      const p = data.progress;
      const etaStr = p.eta_seconds > 0 ? formatETA(p.eta_seconds) : '--';
      liveTelemetryHTML = `
        <div class="modal-live-card">
          <div style="display:flex; justify-content:space-between; align-items:center; margin-bottom:0.6rem;">
            <div style="display:flex; align-items:center; gap:8px;">
              <span class="badge badge-transcoding" style="font-size:0.72rem;">ACTIVE: ${escapeHtml(p.phase.toUpperCase())}</span>
              <span style="font-size:0.8rem; color:var(--text-secondary);">${escapeHtml(p.phase_detail || '')}</span>
            </div>
            ${p.is_stuck ? 
              `<span class="badge-stuck">⚠️ STUCK? (${p.stuck_duration_sec}s silence)</span>` : 
              `<span style="font-size:0.75rem; color:var(--accent-emerald); display:inline-flex; align-items:center; gap:4px;"><span style="display:inline-block; width:7px; height:7px; border-radius:50%; background:#10b981;"></span> Streaming Live Telemetry</span>`}
          </div>
          <div style="margin-bottom:0.75rem;">
            <div style="display:flex; justify-content:space-between; font-size:0.8rem; font-weight:600; margin-bottom:4px;">
              <span style="color:var(--accent-amber);">${p.percentage.toFixed(1)}% Completed</span>
              <span>Estimated Time Remaining: <strong style="color:var(--text-primary);">${etaStr}</strong></span>
            </div>
            <div class="queue-progress-bar-wrap" style="height:7px;">
              <div class="queue-progress-bar-fill ${p.phase === 'verifying_vmaf' ? 'vmaf' : ''} ${p.is_stuck ? 'stuck' : ''}" style="width:${Math.min(100, Math.max(3, p.percentage))}%"></div>
            </div>
          </div>
          <div style="display:grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap:0.6rem; font-size:0.78rem;">
            <div><span style="color:var(--text-muted); font-size:0.7rem;">SPEED</span><br><strong>${p.speed > 0 ? p.speed.toFixed(2) + 'x' : '--'}</strong></div>
            <div><span style="color:var(--text-muted); font-size:0.7rem;">CURRENT FPS</span><br><strong>${p.fps > 0 ? Math.round(p.fps) : '--'}</strong></div>
            <div><span style="color:var(--text-muted); font-size:0.7rem;">ELAPSED</span><br><strong>${formatDuration(p.current_sec)}</strong></div>
            <div><span style="color:var(--text-muted); font-size:0.7rem;">TOTAL DURATION</span><br><strong>${formatDuration(p.total_sec)}</strong></div>
            <div><span style="color:var(--text-muted); font-size:0.7rem;">BITRATE</span><br><strong>${escapeHtml(p.bitrate || '--')}</strong></div>
            <div><span style="color:var(--text-muted); font-size:0.7rem;">FRAMES</span><br><strong>${p.frames ? p.frames.toLocaleString() : '--'}</strong></div>
          </div>
        </div>
      `;
    }

    content.innerHTML = `
      ${liveTelemetryHTML}
      ${data.entry.state === 'leased' ? `
        <div style="margin-bottom:1rem; padding:0.75rem; background:rgba(245,158,11,0.08); border:1px solid rgba(245,158,11,0.25); border-radius:var(--radius-sm); font-size:0.8rem; color:var(--text-secondary);">
          <strong>Stato Riservato (Leased):</strong> Questo file è stato preso in carico dal worker ed è in coda di esecuzione immediata. Se il container è stato appena riavviato, il job è stato recuperato automaticamente e ripartirà da capo dal file originale (senza usare parti incomplete rimaste nello stage).
        </div>` : ''}
      <div style="display:grid; grid-template-columns:1fr 1fr; gap:1rem; margin-bottom:1rem;">
        <div>
          <p style="font-size:0.78rem; color:var(--text-muted);">FULL FILE PATH</p>
          <p style="font-size:0.85rem; word-break:break-all;">${escapeHtml(data.entry.file_path)}</p>
        </div>
        <div>
          <p style="font-size:0.78rem; color:var(--text-muted);">CURRENT STATE</p>
          <p><span class="badge badge-${data.entry.state}">${data.entry.state}</span></p>
        </div>
      </div>
      ${data.entry.error_message ? `
        <div style="margin-bottom:1rem; padding:0.75rem; background:rgba(244,63,94,0.1); border:1px solid rgba(244,63,94,0.3); border-radius:var(--radius-sm); color:#fb7185; font-size:0.82rem;">
          <strong>Error Details:</strong> ${escapeHtml(data.entry.error_message)}
        </div>` : ''}
      ${data.metadata ? `
        <div style="display:grid; grid-template-columns:repeat(auto-fit, minmax(140px, 1fr)); gap:0.75rem; margin-bottom:1rem; background:rgba(0,0,0,0.2); padding:0.85rem; border-radius:var(--radius-sm);">
          <div><span style="color:var(--text-muted); font-size:0.72rem;">CODEC</span><br><strong>${escapeHtml(data.metadata.video_codec)}</strong></div>
          <div><span style="color:var(--text-muted); font-size:0.72rem;">RESOLUTION</span><br><strong>${escapeHtml(data.metadata.resolution)}</strong></div>
          <div><span style="color:var(--text-muted); font-size:0.72rem;">DURATION</span><br><strong>${data.metadata.duration.toFixed(1)}s</strong></div>
          <div><span style="color:var(--text-muted); font-size:0.72rem;">DECISION</span><br><strong>${escapeHtml(data.metadata.decision_action)} (${escapeHtml(data.metadata.preset)})</strong></div>
        </div>
      ` : '<p style="color:var(--text-muted); font-size:0.82rem;">Metadata analysis pending or not available.</p>'}
      ${streamPlanHTML}
    `;
  } catch (e) {
    content.innerHTML = `<div style="color:var(--accent-rose);">Network error loading job details.</div>`;
  }
}

async function requeueJob(id) {
  try {
    const res = await fetch(`/api/queue/${id}/requeue`, { method: "POST" });
    if (res.ok) {
      showToast(`Job #${id} ripristinato in coda`, "success");
      fetchQueue();
    } else {
      showToast("Impossibile ripristinare il job", "error");
    }
  } catch (e) {
    showToast("Errore di rete durante il ripristino", "error");
  }
}

async function pauseJob(id) {
  try {
    const res = await fetch(`/api/queue/${id}/pause`, { method: "POST" });
    if (res.ok) {
      showToast(`Job #${id} messo in pausa (potrai riprenderlo con Resume)`, "success");
      fetchQueue();
    } else {
      showToast("Impossibile mettere in pausa il job", "error");
    }
  } catch (e) {
    showToast("Errore di rete durante la pausa", "error");
  }
}

async function ignoreJob(id) {
  if (!confirm(`Vuoi escludere il Job #${id} dalla coda?\n\nIl file verrà contrassegnato come 'skipped' e lo scanner automatico periodico NON lo reinserirà più.\n\nPotrai riprendere la codifica in qualsiasi momento cliccando 'Resume'.`)) return;
  try {
    const res = await fetch(`/api/queue/${id}/ignore`, { method: "POST" });
    if (res.ok) {
      showToast(`Job #${id} escluso dalla coda (non verrà riprocessato)`, "success");
      fetchQueue();
    } else {
      showToast("Impossibile escludere il job", "error");
    }
  } catch (e) {
    showToast("Errore di rete durante l'esclusione", "error");
  }
}

async function deleteJob(id) {
  if (!confirm(`ATTENZIONE: Stai per cancellare definitivamente il Job #${id} dal database.\n\nSe il file è ancora presente nella cartella monitorata, lo scanner periodico lo rischedulerà al prossimo ciclo.\nSe desideri semplicemente escluderlo dall'encoding, usa il tasto 'Escludi'.\n\nProcedere con l'eliminazione definitiva?`)) return;
  try {
    const res = await fetch(`/api/queue/${id}/delete`, { method: "POST" });
    if (res.ok) {
      showToast(`Job #${id} eliminato definitivamente dal database`, "success");
      fetchQueue();
    } else {
      showToast("Impossibile eliminare il job", "error");
    }
  } catch (e) {
    showToast("Errore di rete durante l'eliminazione", "error");
  }
}

function openPriorityModal(id, currentPriority) {
  selectedPriorityJobId = id;
  const titleEl = document.getElementById("modal-priority-title");
  if (titleEl) titleEl.textContent = `Imposta Priorità Coda (Job #${id})`;

  const prio = currentPriority !== undefined && currentPriority !== null ? currentPriority : 50;
  const input = document.getElementById("priority-custom-input");
  if (input) input.value = prio;

  // Highlight matching preset button if any
  document.querySelectorAll(".priority-preset-btn").forEach(btn => {
    if (btn.getAttribute("data-priority") === String(prio)) {
      btn.classList.add("active");
    } else {
      btn.classList.remove("active");
    }
  });

  openModal("modal-priority");
}

async function savePriority() {
  if (!selectedPriorityJobId) return;
  const input = document.getElementById("priority-custom-input");
  if (!input) return;

  const val = parseInt(input.value, 10);
  if (isNaN(val) || val < 1 || val > 1000) {
    showToast("La priorità deve essere un numero compreso tra 1 e 1000", "error");
    return;
  }

  try {
    const res = await fetch(`/api/queue/${selectedPriorityJobId}/priority`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ priority: val })
    });
    if (res.ok) {
      showToast(`Priorità del Job #${selectedPriorityJobId} impostata a ${val}`, "success");
      closeModal("modal-priority");
      fetchQueue();
    } else {
      const txt = await res.text();
      showToast(`Errore: ${txt || "Impossibile aggiornare la priorità"}`, "error");
    }
  } catch (e) {
    showToast("Errore di rete durante l'aggiornamento della priorità", "error");
  }
}


// ==========================================================================
// API Calls: Configuration & Rules
// ==========================================================================
async function fetchConfig() {
  try {
    const res = await fetch("/api/config");
    if (!res.ok) return;
    currentConfig = await res.json();
    renderConfigForms(currentConfig);
  } catch (e) {
    console.error("Failed to load config:", e);
  }
}

function renderConfigForms(cfg) {
  if (!cfg) return;

  // Render Rules Table
  const rulesTbody = document.getElementById("rules-table-body");
  if (rulesTbody) {
    rulesTbody.innerHTML = "";
    if (cfg.evaluation && cfg.evaluation.rules) {
      cfg.evaluation.rules.forEach((rule, idx) => {
        const tr = document.createElement("tr");
        const conds = Object.entries(rule.conditions || {}).map(([k, v]) => `${k}=${v}`).join(", ") || "All files";
        tr.innerHTML = `
          <td><strong>${rule.priority}</strong></td>
          <td><strong>${escapeHtml(rule.name)}</strong></td>
          <td style="color:var(--text-secondary);">${escapeHtml(conds)}</td>
          <td><span class="badge badge-${rule.action === 'transcode' ? 'completed' : 'skipped'}">${rule.action}</span></td>
          <td>${escapeHtml(rule.preset || "--")}</td>
          <td>${escapeHtml(rule.audio_action || "copy")}</td>
          <td>
            <div style="display:flex; gap:0.4rem;">
              <button class="btn btn-secondary btn-sm" onclick="editRule(${idx})">Edit</button>
              <button class="btn btn-danger btn-sm" onclick="deleteRule(${idx})">Delete</button>
            </div>
          </td>
        `;
        rulesTbody.appendChild(tr);
      });
    }
  }

  // Audio Preservation
  if (cfg.evaluation && cfg.evaluation.audio_preservation) {
    document.getElementById("pres-surround").checked = !!cfg.evaluation.audio_preservation.retain_surround_tracks;
    document.getElementById("pres-lossless").checked = !!cfg.evaluation.audio_preservation.copy_lossless_audio;
    document.getElementById("pres-languages").value = (cfg.evaluation.audio_preservation.preferred_languages || []).join(", ");
  }

  // Render Presets
  const presetsGrid = document.getElementById("presets-grid");
  if (presetsGrid && cfg.transcoder && cfg.transcoder.presets) {
    presetsGrid.innerHTML = "";
    cfg.transcoder.presets.forEach((p, idx) => {
      const card = document.createElement("div");
      card.className = "stat-card";
      card.innerHTML = `
        <div class="stat-header">
          <span class="stat-title">${escapeHtml(p.name)}</span>
          <span class="badge badge-completed">${p.video_codec.toUpperCase()}</span>
        </div>
        <div style="margin:0.75rem 0; font-size:0.85rem; color:var(--text-secondary);">
          <div>CRF Quality: <strong>${p.quality_crf}</strong></div>
          <div>Speed Preset: <strong>${escapeHtml(p.preset_speed)}</strong></div>
          <div>Audio: <strong>${escapeHtml(p.audio_codec)}</strong> (${escapeHtml(p.audio_bitrate || '192k')})</div>
          ${p.extra_ffmpeg ? `<div style="font-size:0.75rem; color:var(--text-muted); margin-top:0.25rem;">${escapeHtml(p.extra_ffmpeg)}</div>` : ''}
        </div>
        <div style="display:flex; gap:0.5rem; margin-top:1rem;">
          <button class="btn btn-secondary btn-sm" onclick="editPreset(${idx})">Edit</button>
          <button class="btn btn-danger btn-sm" onclick="deletePreset(${idx})">Delete</button>
        </div>
      `;
      presetsGrid.appendChild(card);
    });
  }

  // Full Settings Form
  if (cfg.concurrency) {
    document.getElementById("cfg-workers").value = cfg.concurrency.worker_count;
    document.getElementById("cfg-gpu-limit").value = cfg.concurrency.gpu_semaphore_limit;
    document.getElementById("cfg-cpu-limit").value = cfg.concurrency.cpu_semaphore_limit;
    document.getElementById("cfg-prefetch-depth").value = cfg.concurrency.prefetch_buffer_depth;
    document.getElementById("cfg-drain-timeout").value = cfg.concurrency.drain_timeout || "5m";
    document.getElementById("cfg-max-retries").value = cfg.concurrency.retry_max_attempts;
  }

  if (cfg.transcoder) {
    document.getElementById("cfg-hwaccel").value = cfg.transcoder.hardware_acceleration || "auto";
    document.getElementById("cfg-staging-dir").value = cfg.transcoder.staging_dir || "";
    document.getElementById("cfg-vmaf-thresh").value = cfg.transcoder.vmaf_threshold || 93.0;
    document.getElementById("cfg-skip-larger").checked = !!cfg.transcoder.skip_if_larger;
    document.getElementById("cfg-overwrite-source").checked = cfg.transcoder.overwrite_source !== false;
    document.getElementById("cfg-output-suffix").value = cfg.transcoder.output_suffix || "_crunched";
    document.getElementById("cfg-output-dir").value = cfg.transcoder.output_dir || "";
  }

  if (cfg.filesystem && cfg.filesystem.scopes) {
    document.getElementById("cfg-scopes-json").value = JSON.stringify(cfg.filesystem.scopes, null, 2);
  }
}

// ==========================================================================
// Rule & Preset Dialog Actions
// ==========================================================================
function editRule(idx) {
  if (!currentConfig || !currentConfig.evaluation || !currentConfig.evaluation.rules[idx]) return;
  editingRuleIndex = idx;
  const r = currentConfig.evaluation.rules[idx];

  document.getElementById("modal-rule-title").textContent = `Edit Rule: ${r.name}`;
  document.getElementById("rule-name").value = r.name;
  document.getElementById("rule-priority").value = r.priority;
  document.getElementById("rule-action").value = r.action;
  document.getElementById("rule-preset").value = r.preset || "";
  document.getElementById("rule-cond-codec").value = (r.conditions && r.conditions.codec) || "";
  document.getElementById("rule-cond-res").value = (r.conditions && r.conditions.resolution) || "";
  openModal("modal-rule");
}

function deleteRule(idx) {
  if (!confirm("Delete this rule?")) return;
  currentConfig.evaluation.rules.splice(idx, 1);
  saveConfigDirect(currentConfig, "Rule deleted successfully");
}

function saveRuleFromModal() {
  const name = document.getElementById("rule-name").value.trim();
  const priority = parseInt(document.getElementById("rule-priority").value, 10) || 50;
  const action = document.getElementById("rule-action").value;
  const preset = document.getElementById("rule-preset").value.trim();
  const codec = document.getElementById("rule-cond-codec").value.trim();
  const res = document.getElementById("rule-cond-res").value.trim();

  if (!name) {
    alert("Rule name is required.");
    return;
  }

  const conds = {};
  if (codec) conds.codec = codec;
  if (res) conds.resolution = res;

  const ruleObj = {
    name: name,
    priority: priority,
    action: action,
    preset: preset,
    conditions: conds,
    audio_action: "copy",
    retain_subtitles: true
  };

  if (!currentConfig.evaluation) currentConfig.evaluation = { rules: [] };
  if (!currentConfig.evaluation.rules) currentConfig.evaluation.rules = [];

  if (editingRuleIndex >= 0) {
    currentConfig.evaluation.rules[editingRuleIndex] = ruleObj;
  } else {
    currentConfig.evaluation.rules.push(ruleObj);
  }

  closeModal("modal-rule");
  saveConfigDirect(currentConfig, "Rule saved successfully");
}

function editPreset(idx) {
  if (!currentConfig || !currentConfig.transcoder || !currentConfig.transcoder.presets[idx]) return;
  editingPresetIndex = idx;
  const p = currentConfig.transcoder.presets[idx];

  document.getElementById("modal-preset-title").textContent = `Edit Preset: ${p.name}`;
  document.getElementById("preset-name").value = p.name;
  document.getElementById("preset-codec").value = p.video_codec;
  document.getElementById("preset-crf").value = p.quality_crf;
  document.getElementById("preset-speed").value = p.preset_speed;
  document.getElementById("preset-audio-codec").value = p.audio_codec;
  document.getElementById("preset-extra").value = p.extra_ffmpeg || "";
  openModal("modal-preset");
}

function deletePreset(idx) {
  if (!confirm("Delete this preset?")) return;
  currentConfig.transcoder.presets.splice(idx, 1);
  saveConfigDirect(currentConfig, "Preset deleted successfully");
}

function savePresetFromModal() {
  const name = document.getElementById("preset-name").value.trim();
  const codec = document.getElementById("preset-codec").value;
  const crf = parseInt(document.getElementById("preset-crf").value, 10) || 22;
  const speed = document.getElementById("preset-speed").value.trim() || "medium";
  const audioCodec = document.getElementById("preset-audio-codec").value.trim() || "copy";
  const extra = document.getElementById("preset-extra").value.trim();

  if (!name) {
    alert("Preset name is required.");
    return;
  }

  const presetObj = {
    name: name,
    video_codec: codec,
    quality_crf: crf,
    preset_speed: speed,
    audio_codec: audioCodec,
    audio_bitrate: "192k",
    extra_ffmpeg: extra
  };

  if (!currentConfig.transcoder) currentConfig.transcoder = { presets: [] };
  if (!currentConfig.transcoder.presets) currentConfig.transcoder.presets = [];

  if (editingPresetIndex >= 0) {
    currentConfig.transcoder.presets[editingPresetIndex] = presetObj;
  } else {
    currentConfig.transcoder.presets.push(presetObj);
  }

  closeModal("modal-preset");
  saveConfigDirect(currentConfig, "Preset saved successfully");
}

function savePreservation() {
  if (!currentConfig.evaluation) currentConfig.evaluation = {};
  if (!currentConfig.evaluation.audio_preservation) currentConfig.evaluation.audio_preservation = {};

  currentConfig.evaluation.audio_preservation.retain_surround_tracks = document.getElementById("pres-surround").checked;
  currentConfig.evaluation.audio_preservation.copy_lossless_audio = document.getElementById("pres-lossless").checked;

  const langs = document.getElementById("pres-languages").value.split(",").map(s => s.trim().toLowerCase()).filter(Boolean);
  currentConfig.evaluation.audio_preservation.preferred_languages = langs;

  saveConfigDirect(currentConfig, "Audio preservation directives updated");
}

function saveAllSettings() {
  try {
    currentConfig.concurrency.worker_count = parseInt(document.getElementById("cfg-workers").value, 10);
    currentConfig.concurrency.gpu_semaphore_limit = parseInt(document.getElementById("cfg-gpu-limit").value, 10);
    currentConfig.concurrency.cpu_semaphore_limit = parseInt(document.getElementById("cfg-cpu-limit").value, 10);
    currentConfig.concurrency.prefetch_buffer_depth = parseInt(document.getElementById("cfg-prefetch-depth").value, 10);
    currentConfig.concurrency.retry_max_attempts = parseInt(document.getElementById("cfg-max-retries").value, 10);

    currentConfig.transcoder.hardware_acceleration = document.getElementById("cfg-hwaccel").value;
    currentConfig.transcoder.staging_dir = document.getElementById("cfg-staging-dir").value.trim();
    currentConfig.transcoder.vmaf_threshold = parseFloat(document.getElementById("cfg-vmaf-thresh").value);
    currentConfig.transcoder.skip_if_larger = document.getElementById("cfg-skip-larger").checked;
    currentConfig.transcoder.overwrite_source = document.getElementById("cfg-overwrite-source").checked;
    currentConfig.transcoder.output_suffix = document.getElementById("cfg-output-suffix").value.trim() || "_crunched";
    currentConfig.transcoder.output_dir = document.getElementById("cfg-output-dir").value.trim();

    const scopesJSON = document.getElementById("cfg-scopes-json").value.trim();
    if (scopesJSON) {
      currentConfig.filesystem.scopes = JSON.parse(scopesJSON);
    }

    saveConfigDirect(currentConfig, "System configuration updated & applied live");
  } catch (e) {
    alert("Invalid JSON format in Filesystem Scopes: " + e.message);
  }
}

async function saveConfigDirect(cfg, successMsg) {
  try {
    const res = await fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(cfg)
    });
    if (res.ok) {
      showToast(successMsg, "success");
      fetchConfig();
    } else {
      showToast("Error applying configuration updates", "error");
    }
  } catch (e) {
    showToast("Network error saving configuration", "error");
  }
}

// ==========================================================================
// Hardware Capability Detection
// ==========================================================================
async function fetchHardware() {
  const box = document.getElementById("hw-capabilities-box");
  if (!box) return;
  try {
    const res = await fetch("/api/hardware");
    if (!res.ok) return;
    const data = await res.json();
    box.innerHTML = `
      <strong>Detected Hardware Drivers:</strong><br>
      • NVIDIA NVENC: <span style="color:${data.has_nvenc ? '#34d399' : '#64748b'}">${data.has_nvenc ? 'Detected' : 'Not Present'}</span><br>
      • Intel QuickSync: <span style="color:${data.has_qsv ? '#34d399' : '#64748b'}">${data.has_qsv ? 'Detected' : 'Not Present'}</span><br>
      • AMD AMF: <span style="color:${data.has_amf ? '#34d399' : '#64748b'}">${data.has_amf ? 'Detected' : 'Not Present'}</span><br>
      • Linux VAAPI: <span style="color:${data.has_vaapi ? '#34d399' : '#64748b'}">${data.has_vaapi ? 'Detected' : 'Not Present'}</span><br>
      <div style="margin-top:0.5rem; color:var(--text-muted);">Available FFmpeg Encoders: ${data.encoders.join(", ")}</div>
    `;
  } catch (e) {
    box.textContent = "Unable to query hardware profiles.";
  }
}

// ==========================================================================
// Utilities
// ==========================================================================
function formatBytes(b) {
  if (!b || b === 0) return "0 B";
  const unit = 1024;
  if (b < unit) return b + " B";
  const exp = Math.floor(Math.log(b) / Math.log(unit));
  const pre = "KMGTPE"[exp - 1];
  return (b / Math.pow(unit, exp)).toFixed(2) + " " + pre + "B";
}

function formatDuration(sec) {
  if (!sec || sec < 0) return "00:00";
  const s = Math.floor(sec);
  const m = Math.floor(s / 60);
  const h = Math.floor(m / 60);
  const remM = m % 60;
  const remS = s % 60;
  if (h > 0) {
    return `${h}:${String(remM).padStart(2, '0')}:${String(remS).padStart(2, '0')}`;
  }
  return `${String(remM).padStart(2, '0')}:${String(remS).padStart(2, '0')}`;
}

function formatETA(sec) {
  if (sec === undefined || sec === null || sec < 0) return "--";
  if (sec === 0) return "Done";
  if (sec < 60) return `~${sec}s`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (m < 60) {
    return s > 0 ? `~${m}m ${s}s` : `~${m}m`;
  }
  const h = Math.floor(m / 60);
  const remM = m % 60;
  return `~${h}h ${remM}m`;
}

function escapeHtml(str) {
  if (!str) return "";
  return String(str)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

function showToast(message, type = "success") {
  const container = document.getElementById("toast-container");
  if (!container) return;

  const toast = document.createElement("div");
  toast.className = `toast ${type}`;
  toast.innerHTML = `
    <span>${type === 'success' ? '✓' : '⚠'}</span>
    <span>${escapeHtml(message)}</span>
  `;

  container.appendChild(toast);
  setTimeout(() => {
    toast.style.opacity = "0";
    toast.style.transition = "opacity 0.3s ease";
    setTimeout(() => toast.remove(), 300);
  }, 3500);
}
