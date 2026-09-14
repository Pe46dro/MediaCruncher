// MediaCruncher Real-Time Web Dashboard Client
(() => {
  'use strict';

  // State
  let currentFilter = 'all';
  let searchQuery = '';
  let currentPage = 1;
  const pageSize = 20;
  let totalMediaCount = 0;
  let sseSource = null;

  // DOM Elements
  const daemonStatusBadge = document.getElementById('daemonStatusBadge');
  const daemonStatusText = document.getElementById('daemonStatusText');
  const hwAccelVal = document.getElementById('hwAccelVal');
  const uptimeVal = document.getElementById('uptimeVal');
  const btnTriggerScan = document.getElementById('btnTriggerScan');

  // KPI Elements
  const kpiActiveJobs = document.getElementById('kpiActiveJobs');
  const kpiWorkerCount = document.getElementById('kpiWorkerCount');
  const kpiQueueDepth = document.getElementById('kpiQueueDepth');
  const kpiCompleted = document.getElementById('kpiCompleted');
  const kpiQualityKept = document.getElementById('kpiQualityKept');
  const kpiSpaceSaved = document.getElementById('kpiSpaceSaved');
  const kpiBytesSaved = document.getElementById('kpiBytesSaved');
  const kpiAvgVMAF = document.getElementById('kpiAvgVMAF');
  const kpiVMAFThresholdNote = document.getElementById('kpiVMAFThresholdNote');

  // Active Jobs
  const activeJobsContainer = document.getElementById('activeJobsContainer');
  const activeJobsCountBadge = document.getElementById('activeJobsCountBadge');

  // Media Table
  const mediaTableBody = document.getElementById('mediaTableBody');
  const mediaSearchInput = document.getElementById('mediaSearchInput');
  const statusFilterPills = document.getElementById('statusFilterPills');
  const countAll = document.getElementById('countAll');
  const countCompleted = document.getElementById('countCompleted');
  const countSkippedQuality = document.getElementById('countSkippedQuality');
  const countIgnored = document.getElementById('countIgnored');
  const countFailed = document.getElementById('countFailed');
  const paginationInfo = document.getElementById('paginationInfo');
  const btnPrevPage = document.getElementById('btnPrevPage');
  const btnNextPage = document.getElementById('btnNextPage');

  // Terminal Logs
  const terminalLogs = document.getElementById('terminalLogs');
  const terminalWindow = document.getElementById('terminalWindow');
  const chkAutoScroll = document.getElementById('chkAutoScroll');
  const btnClearLog = document.getElementById('btnClearLog');

  // --------------------------------------------------------------------------
  // Helper Functions
  // --------------------------------------------------------------------------
  function formatBytes(bytes) {
    if (!bytes || bytes <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(1024));
    return (bytes / Math.pow(1024, i)).toFixed(2) + ' ' + units[i];
  }

  function formatDuration(seconds) {
    if (!seconds || seconds <= 0) return '0s';
    const hrs = Math.floor(seconds / 3600);
    const mins = Math.floor((seconds % 3600) / 60);
    const secs = Math.floor(seconds % 60);
    if (hrs > 0) return `${hrs}h ${mins}m ${secs}s`;
    if (mins > 0) return `${mins}m ${secs}s`;
    return `${secs}s`;
  }

  function formatTime(isoString) {
    if (!isoString) return '-';
    const d = new Date(isoString);
    return isNaN(d.getTime()) ? isoString : d.toLocaleTimeString();
  }

  function formatDate(isoString) {
    if (!isoString) return '-';
    const d = new Date(isoString);
    return isNaN(d.getTime()) ? isoString : d.toLocaleString();
  }

  function appendLog(severity, badgeText, message) {
    const now = new Date().toTimeString().split(' ')[0];
    const entry = document.createElement('div');
    entry.className = `log-entry log-${severity}`;
    
    let badgeClass = 'badge-info';
    if (severity === 'success') badgeClass = 'badge-success';
    if (severity === 'warning') badgeClass = 'badge-warning';
    if (severity === 'error') badgeClass = 'badge-error';

    entry.innerHTML = `
      <span class="log-time">[${now}]</span>
      <span class="log-badge ${badgeClass}">${badgeText}</span>
      <span class="log-msg">${escapeHtml(message)}</span>
    `;

    terminalLogs.appendChild(entry);

    if (chkAutoScroll.checked) {
      terminalWindow.scrollTop = terminalWindow.scrollHeight;
    }
  }

  function escapeHtml(str) {
    if (!str) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  // --------------------------------------------------------------------------
  // Fetch System Status & Metrics
  // --------------------------------------------------------------------------
  async function fetchStatus() {
    try {
      const res = await fetch('/api/status');
      if (!res.ok) return;
      const data = await res.json();

      // Daemon status badge
      const status = data.status || 'idle';
      daemonStatusText.textContent = status.toUpperCase();
      if (status === 'processing') {
        daemonStatusBadge.style.color = '#06B6D4';
        daemonStatusBadge.style.borderColor = 'rgba(6, 182, 212, 0.4)';
        daemonStatusBadge.style.background = 'rgba(6, 182, 212, 0.1)';
      } else {
        daemonStatusBadge.style.color = '#10B981';
        daemonStatusBadge.style.borderColor = 'rgba(16, 185, 129, 0.25)';
        daemonStatusBadge.style.background = 'rgba(16, 185, 129, 0.1)';
      }

      // Metadata chips
      hwAccelVal.textContent = data.hardware_accel || 'SW / CPU';
      uptimeVal.textContent = formatDuration(data.uptime_seconds);

      // KPI cards
      kpiActiveJobs.textContent = data.active_jobs || 0;
      kpiWorkerCount.textContent = `${data.workers || 4} worker attivi`;
      kpiQueueDepth.textContent = data.queue_depth || 0;

      // Stats from SQLite
      if (data.stats) {
        kpiCompleted.textContent = data.stats.completed || 0;
        kpiQualityKept.textContent = data.stats.skipped_quality || 0;
        kpiSpaceSaved.textContent = (data.stats.savings_percentage || 0).toFixed(1) + '%';
        kpiBytesSaved.textContent = `${formatBytes(data.stats.bytes_saved || 0)} risparmiati`;
        kpiAvgVMAF.textContent = (data.stats.avg_vmaf || 0).toFixed(1);

        // Filter counts
        countAll.textContent = data.stats.total_files || 0;
        countCompleted.textContent = data.stats.completed || 0;
        countSkippedQuality.textContent = data.stats.skipped_quality || 0;
        countIgnored.textContent = data.stats.ignored || 0;
        countFailed.textContent = data.stats.failed || 0;
      }

      if (data.vmaf_threshold) {
        kpiVMAFThresholdNote.textContent = `Soglia min: ${data.vmaf_threshold.toFixed(1)}`;
      }
    } catch (err) {
      console.error('Failed to fetch status:', err);
    }
  }

  // --------------------------------------------------------------------------
  // Fetch Active Jobs
  // --------------------------------------------------------------------------
  async function fetchActiveJobs() {
    try {
      const res = await fetch('/api/jobs');
      if (!res.ok) return;
      const jobs = await res.json();

      activeJobsCountBadge.textContent = `${jobs.length} attive`;

      if (!jobs || jobs.length === 0) {
        activeJobsContainer.innerHTML = `
          <div class="empty-state" id="emptyActiveJobs">
            <svg class="empty-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5">
              <circle cx="12" cy="12" r="10"></circle>
              <polyline points="12 6 12 12 14 14"></polyline>
            </svg>
            <p class="empty-text">Nessuna transcodifica attiva al momento. Il daemon è in attesa di nuovi video.</p>
          </div>
        `;
        return;
      }

      activeJobsContainer.innerHTML = jobs.map(job => {
        const durationSec = Math.floor((job.duration_ms || 0) / 1000);
        const stageText = (job.stage || 'Transcodifica').toUpperCase();
        const prog = Math.min(100, Math.max(5, job.estimated_progress || 15));

        return `
          <div class="active-job-card">
            <div class="job-meta-row">
              <div class="job-file-info">
                <span class="job-badge">${escapeHtml(job.worker_id || 'W1')}</span>
                <span class="job-filename">${escapeHtml(job.file_name || job.source_path)}</span>
              </div>
              <div class="meta-chip">
                <span class="chip-label">DEST:</span>
                <span class="chip-value">${escapeHtml(job.target_codec || 'H.265')} (${escapeHtml(job.preset || 'medium')})</span>
              </div>
            </div>
            <div class="job-progress-wrapper">
              <div class="progress-bar-bg">
                <div class="progress-bar-fill" style="width: ${prog}%"></div>
              </div>
              <div class="progress-labels">
                <span>Fase: <strong>${escapeHtml(stageText)}</strong></span>
                <span>Tempo trascorso: <strong>${formatDuration(durationSec)}</strong></span>
              </div>
            </div>
          </div>
        `;
      }).join('');
    } catch (err) {
      console.error('Failed to fetch active jobs:', err);
    }
  }

  // --------------------------------------------------------------------------
  // Fetch Processed Media Records Table
  // --------------------------------------------------------------------------
  async function fetchMediaTable() {
    try {
      const offset = (currentPage - 1) * pageSize;
      let url = `/api/media?limit=${pageSize}&offset=${offset}`;
      if (currentFilter !== 'all') {
        url += `&status=${encodeURIComponent(currentFilter)}`;
      }

      const res = await fetch(url);
      if (!res.ok) return;
      const result = await res.json();

      const items = result.records || [];
      totalMediaCount = result.total || 0;

      // Filter by search client-side if needed
      let filtered = items;
      if (searchQuery.trim() !== '') {
        const q = searchQuery.toLowerCase();
        filtered = items.filter(it =>
          (it.source_path && it.source_path.toLowerCase().includes(q)) ||
          (it.file_hash && it.file_hash.toLowerCase().includes(q))
        );
      }

      if (filtered.length === 0) {
        mediaTableBody.innerHTML = `
          <tr>
            <td colspan="6" class="table-loading">Nessun video trovato per i criteri selezionati.</td>
          </tr>
        `;
      } else {
        mediaTableBody.innerHTML = filtered.map(item => {
          let badgeHtml = '';
          if (item.status === 'completed') {
            if (item.replaced_original) {
              badgeHtml = '<span class="badge badge-completed">Completato</span> <span class="badge badge-replaced">Sostituito</span>';
            } else {
              badgeHtml = '<span class="badge badge-completed">Completato</span>';
            }
          } else if (item.status === 'skipped_quality') {
            badgeHtml = '<span class="badge badge-skipped-quality">Qualità Preservata</span>';
          } else if (item.status === 'skipped_larger') {
            badgeHtml = '<span class="badge badge-skipped-quality">Dimensione Ottimale</span>';
          } else if (item.status === 'ignored') {
            badgeHtml = '<span class="badge badge-ignored">Conforme/Ignorato</span>';
          } else {
            badgeHtml = `<span class="badge badge-failed">${escapeHtml(item.status)}</span>`;
          }

          const fileName = item.source_path.split('/').pop().split('\\').pop();
          const hashShort = item.file_hash ? item.file_hash.substring(0, 16) + '...' : '-';
          const vmafDisplay = item.vmaf_score > 0 ? item.vmaf_score.toFixed(2) : '-';

          let note = '-';
          if (item.status === 'skipped_quality') {
            note = '<span style="color: #FBBF24;">Originale mantenuto (perdita > soglia)</span>';
          } else if (item.status === 'skipped_larger') {
            note = '<span style="color: #FBBF24;">Originale mantenuto (output risulterebbe più grande)</span>';
          } else if (item.replaced_original) {
            note = '<span style="color: #C084FC; font-weight: 600;">Originale sostituito sul posto</span>';
          } else if (item.output_path) {
            const outName = item.output_path.split('/').pop().split('\\').pop();
            note = `<span style="color: #34D399;">${escapeHtml(outName)}</span>`;
          } else if (item.error_message) {
            note = `<span style="color: #F87171;">${escapeHtml(item.error_message)}</span>`;
          }

          let sizeDisplay = formatBytes(item.file_size);
          if (item.status === 'completed' && item.output_size && item.output_size > 0 && item.output_size < item.file_size) {
            const saved = item.file_size - item.output_size;
            sizeDisplay = `<div style="font-size: 13px;">${formatBytes(item.file_size)} → ${formatBytes(item.output_size)}</div><div style="color: #34D399; font-size: 11px; font-weight: 600;">-${formatBytes(saved)} (${((saved / item.file_size) * 100).toFixed(1)}%)</div>`;
          }

          return `
            <tr>
              <td>
                <div class="file-cell">
                  <span class="file-name" title="${escapeHtml(item.source_path)}">${escapeHtml(fileName)}</span>
                  <span class="file-hash-tag" title="${escapeHtml(item.file_hash)}">HASH: ${escapeHtml(hashShort)}</span>
                </div>
              </td>
              <td>${sizeDisplay}</td>
              <td>${badgeHtml}</td>
              <td><strong>${vmafDisplay}</strong></td>
              <td>${note}</td>
              <td>${formatDate(item.updated_at)}</td>
            </tr>
          `;
        }).join('');
      }

      // Pagination controls
      const maxPages = Math.ceil(totalMediaCount / pageSize) || 1;
      paginationInfo.textContent = `Pagina ${currentPage} di ${maxPages} (${totalMediaCount} file totali)`;
      btnPrevPage.disabled = currentPage <= 1;
      btnNextPage.disabled = currentPage >= maxPages;
    } catch (err) {
      console.error('Failed to fetch media table:', err);
    }
  }

  // --------------------------------------------------------------------------
  // Server-Sent Events (SSE) Listener
  // --------------------------------------------------------------------------
  function initSSE() {
    if (sseSource) {
      sseSource.close();
    }

    sseSource = new EventSource('/api/events');

    sseSource.onopen = () => {
      appendLog('info', 'SSE', 'Connessione stream eventi stabilita con successo.');
    };

    sseSource.onerror = () => {
      daemonStatusText.textContent = 'RICONNESSIONE...';
      daemonStatusBadge.style.color = '#F59E0B';
      daemonStatusBadge.style.borderColor = 'rgba(245, 158, 11, 0.4)';
    };

    // Generic event handler
    sseSource.onmessage = (event) => {
      try {
        const payload = JSON.parse(event.data);
        handleStreamEvent(payload);
      } catch (e) {
        // Ping or raw message
      }
    };

    // Specific custom event listeners
    const eventTypes = [
      'scan_started',
      'scan_completed',
      'file_discovered',
      'file_skipped_hash',
      'transcode_started',
      'transcode_progress',
      'quality_rejected',
      'transcode_completed',
      'transcode_failed',
    ];

    eventTypes.forEach(evtType => {
      sseSource.addEventListener(evtType, (event) => {
        try {
          const payload = JSON.parse(event.data);
          handleStreamEvent(payload);
        } catch (e) {
          console.error('Error parsing SSE event:', e);
        }
      });
    });
  }

  function handleStreamEvent(payload) {
    const sev = payload.severity || 'info';
    const type = (payload.type || 'EVT').toUpperCase();
    const msg = payload.message || JSON.stringify(payload);

    appendLog(sev, type, msg);

    // Refresh UI data on significant events
    if (payload.type === 'transcode_started' || payload.type === 'transcode_progress') {
      fetchActiveJobs();
      fetchStatus();
    } else if (
      payload.type === 'transcode_completed' ||
      payload.type === 'quality_rejected' ||
      payload.type === 'transcode_failed' ||
      payload.type === 'scan_completed'
    ) {
      fetchActiveJobs();
      fetchStatus();
      fetchMediaTable();
    }
  }

  // --------------------------------------------------------------------------
  // Event Listeners
  // --------------------------------------------------------------------------
  statusFilterPills.addEventListener('click', (e) => {
    const pill = e.target.closest('.pill');
    if (!pill) return;
    statusFilterPills.querySelectorAll('.pill').forEach(p => p.classList.remove('active'));
    pill.classList.add('active');
    currentFilter = pill.dataset.filter || 'all';
    currentPage = 1;
    fetchMediaTable();
  });

  mediaSearchInput.addEventListener('input', (e) => {
    searchQuery = e.target.value;
    fetchMediaTable();
  });

  btnPrevPage.addEventListener('click', () => {
    if (currentPage > 1) {
      currentPage--;
      fetchMediaTable();
    }
  });

  btnNextPage.addEventListener('click', () => {
    currentPage++;
    fetchMediaTable();
  });

  btnClearLog.addEventListener('click', () => {
    terminalLogs.innerHTML = '';
    appendLog('info', 'SYS', 'Terminale eventi pulito.');
  });

  btnTriggerScan.addEventListener('click', async () => {
    btnTriggerScan.disabled = true;
    appendLog('info', 'SCAN', 'Richiesta di scansione manuale inviata al daemon...');
    try {
      const res = await fetch('/api/scan', { method: 'POST' });
      if (res.ok) {
        appendLog('success', 'SCAN', 'Scansione avviata con successo.');
      } else {
        appendLog('error', 'SCAN', 'Errore durante l\'avvio della scansione.');
      }
    } catch (err) {
      appendLog('error', 'SCAN', 'Impossibile contattare il daemon per la scansione.');
    } finally {
      setTimeout(() => { btnTriggerScan.disabled = false; }, 3000);
    }
  });

  // --------------------------------------------------------------------------
  // Initialize
  // --------------------------------------------------------------------------
  fetchStatus();
  fetchActiveJobs();
  fetchMediaTable();
  initSSE();

  // Periodic fallback refresh (every 5 seconds)
  setInterval(() => {
    fetchStatus();
    fetchActiveJobs();
  }, 5000);

})();
