(() => {
  const $ = s => document.querySelector(s);
  const $$ = s => [...document.querySelectorAll(s)];

  let token = '';
  let snapshot = null;
  let lastApprovalKey = '';
  let toastTimer;
  let lastSaveSequence = null;
  let requiredSessionDialog = false;
  let inferenceTab = 'groups';
  let activeSessionID = '';
  let initialSetupComplete = false;
  let memoryTreeFetched = false;
  let lastSnapshotVersion = null;
  let globalExecutionLog = [];
  let globalExecutionLogLastId = 0;
  let appVersion = 'Unknown';

  const expandedItems = new Set();
  const renderSignatures = { chat: '', plan: '', context: '', memory: '', log: '', sessions: '', inference: '', graph: '' };
  const logCategories = ['tool', 'memory_lookup', 'indexing', 'negotiation', 'system', 'plan'];
  const activeLogFilters = new Set(logCategories);
  const permissionTools = ['read_file','read_range','search_path','search_text','find_symbol','list_symbols','write_file','patch_file','create_directory','delete_file','delete_directory','execute_process','execute_shell','git_status','git_diff','read_tool_output'];

  const inferenceGroupOrder = [
    'Intent / conversation',
    'Planning',
    'Execution',
    'Evaluation / reflection',
    'Repair',
    'Indexing',
    'Context / memory',
    'Recovery / routing',
    'Finalization'
  ];
  const inferenceGroups = {
    'InterpretIntent': 'Intent / conversation',
    'ResolveAmbiguity': 'Intent / conversation',
    'Discuss': 'Intent / conversation',
    'AskUser': 'Intent / conversation',
    'EvaluateUserAnswer': 'Intent / conversation',
    'DefineAcceptanceCriteria': 'Planning',
    'PlanTask': 'Planning',
    'EvaluatePlan': 'Planning',
    'EvaluateStep': 'Planning',
    'DecomposeStep': 'Planning',
    'Replan': 'Planning',
    'ExecuteStep': 'Execution',
    'EvaluateActionResult': 'Evaluation / reflection',
    'EvaluateStepResult': 'Evaluation / reflection',
    'FinalTaskEvaluation': 'Evaluation / reflection',
    'JSONRepair': 'Repair',
    'NarrativeFrame': 'Indexing',
    'ExtractKeywords': 'Indexing',
    'ExtractDurableMemory': 'Context / memory',
    'ConsolidateContext': 'Context / memory',
    'RerankMemory': 'Context / memory',
    'ExtractStepResult': 'Context / memory',
    'RefreshTaskContext': 'Context / memory',
    'AssessTaskRecovery': 'Recovery / routing',
    'EvaluateIntervention': 'Recovery / routing',
    'RouteNext': 'Recovery / routing',
    'SummarizeTask': 'Finalization',
    'GenerateFinalResponse': 'Finalization'
  };

  async function api(path, opts = {}) {
    const headers = {...(opts.headers || {})};
    if (opts.method && opts.method !== 'GET') headers['X-Loa-Token'] = token;
    if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(path, {...opts, headers});
    let data = {};
    try { data = await res.json(); } catch {}
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data;
  }

  function toast(msg, bad = false) {
    const el = $('#toast');
    el.textContent = msg;
    el.className = 'toast show' + (bad ? ' error' : '');
    if (el.showPopover && !el.matches(':popover-open')) {
      try { el.showPopover(); } catch (e) {}
    }
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      el.className = 'toast';
      if (el.hidePopover) {
        setTimeout(() => { try { el.hidePopover(); } catch (e) {} }, 250);
      }
    }, 3500);
  }

  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const pretty = v => JSON.stringify(v, null, 2);
  const dt = s => { try { return new Date(s).toLocaleTimeString(); } catch { return ''; } };
  const dateTime = s => { try { return new Date(s).toLocaleString(); } catch { return ''; } };
  const signature = v => JSON.stringify(v ?? null);

  async function bootstrap() {
    const b = await api('/api/bootstrap');
    token = b.token;
    appVersion = b.version || 'Unknown';
    $('#projectRoot').textContent = b.project_root;
    initResizers();
    initInferenceTooltip();
    await refresh();
    setInterval(refresh, 800);
  }

  // --- Memory Viewer Logic ---

  async function fetchMemoryTree() {
    try {
      const res = await fetch('/api/memory/tree');
      if (!res.ok) throw new Error('Failed to fetch memory tree');
      const files = await res.json();
      renderMemoryTree(files);
    } catch (err) {
      console.error(err);
      $('#fileTree').innerHTML = '<div class="empty">Failed to load tree</div>';
    }
  }

  function renderMemoryTree(files) {
    const tree = {};
    for (const file of files) {
      const parts = file.split('/');
      let curr = tree;
      for (let i = 0; i < parts.length; i++) {
        const part = parts[i];
        if (!curr[part]) curr[part] = i === parts.length - 1 ? null : {};
        curr = curr[part];
      }
    }

    const html = [`<div class="file-tree-node special-node" data-path="__GLOBAL__" style="margin-bottom: 8px; border-bottom: 1px solid var(--line); padding-bottom: 8px;">🌐 GLOBAL KNOWLEDGE</div>`];
    function walk(node, path) {
      for (const [k, v] of Object.entries(node).sort((a,b) => (a[1]===null)-(b[1]===null) || a[0].localeCompare(b[0]))) {
        const curPath = path ? path + '/' + k : k;
        if (v === null) {
          html.push(`<div class="file-tree-node" data-path="${curPath}">📄 ${k}</div>`);
        } else {
          html.push(`<details class="file-tree-dir-details" open><summary class="file-tree-dir">📁 ${k}</summary><div style="margin-left:10px;">`);
          walk(v, curPath);
          html.push(`</div></details>`);
        }
      }
    }
    walk(tree, '');
    $('#fileTree').innerHTML = html.join('');

    $$('#fileTree .file-tree-node').forEach(el => {
      el.addEventListener('click', () => {
        $$('#fileTree .file-tree-node').forEach(x => x.classList.remove('active'));
        el.classList.add('active');
        fetchMemoryFile(el.dataset.path);
      });
    });
  }

  async function fetchMemoryFile(path) {
    $('#fileContent').innerHTML = '<div class="empty">Loading...</div>';
    try {
      const res = await fetch('/api/memory/file?path=' + encodeURIComponent(path));
      if (!res.ok) throw new Error('Failed to fetch file details');
      const data = await res.json();
      renderMemoryFile(path, data);
    } catch (err) {
      console.error(err);
      $('#fileContent').innerHTML = '<div class="empty">Failed to load details</div>';
    }
  }
  window.fetchMemoryFile = fetchMemoryFile;

  function renderArtifactList(artifactsMap) {
    const listEl = $('#artifactList');
    if (!artifactsMap || Object.keys(artifactsMap).length === 0) {
      listEl.innerHTML = '<div class="empty">No artifacts found</div>';
      return;
    }
    
    const taskIds = Object.keys(artifactsMap).map(Number).sort((a, b) => b - a); // descending
    const html = [];
    
    taskIds.forEach(taskId => {
      const files = artifactsMap[taskId];
      if (!files || files.length === 0) return;
      
      html.push(`<div class="file-tree-dir expanded" style="cursor:pointer; padding:4px 8px; font-weight:bold; color:var(--text-bright);">
        <span class="chevron" style="display:inline-block; width:12px;">▼</span> 📁 Plan ${taskId}
      </div>`);
      
      files.sort().forEach(name => {
        html.push(`<div class="file-tree-node" data-name="${escapeHTML(name)}" data-task="${taskId}" style="padding-left: 24px;">📄 ${escapeHTML(name)}</div>`);
      });
    });
    
    listEl.innerHTML = html.join('');

    $$('#artifactList .file-tree-dir').forEach(el => {
      el.addEventListener('click', () => {
        el.classList.toggle('expanded');
        const isExpanded = el.classList.contains('expanded');
        el.querySelector('.chevron').textContent = isExpanded ? '▼' : '▶';
        let next = el.nextElementSibling;
        while (next && next.classList.contains('file-tree-node')) {
          next.style.display = isExpanded ? 'block' : 'none';
          next = next.nextElementSibling;
        }
      });
    });

    $$('#artifactList .file-tree-node').forEach(el => {
      el.addEventListener('click', () => {
        $$('#artifactList .file-tree-node').forEach(x => x.classList.remove('active'));
        el.classList.add('active');
        fetchArtifactContent(el.dataset.name, el.dataset.task);
      });
    });
  }

  async function fetchArtifactContent(name, taskId) {
    $('#artifactContent').innerHTML = '<div class="empty">Loading...</div>';
    try {
      const res = await fetch('/api/artifact?task_id=' + encodeURIComponent(taskId) + '&name=' + encodeURIComponent(name));
      if (!res.ok) throw new Error('Failed to fetch artifact content');
      const text = await res.text();
      const controlsHTML = `
        <div style="display:flex; justify-content:flex-end; padding: 10px 15px 0 15px; gap: 8px;">
           <button class="artifact-btn" id="copyArtifactBtn" title="Copy Raw Source">
             <svg viewBox="0 0 24 24" width="14" height="14" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>
           </button>
           <button class="artifact-btn" id="downloadArtifactBtn" title="Download File">
             <svg viewBox="0 0 24 24" width="14" height="14" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"></path><polyline points="7 10 12 15 17 10"></polyline><line x1="12" y1="15" x2="12" y2="3"></line></svg>
           </button>
        </div>
      `;

      if (name.endsWith('.md') && typeof marked !== 'undefined') {
        const html = marked.parse(text);
        $('#artifactContent').innerHTML = controlsHTML + `<div class="markdown-body" style="padding-top:5px;">${html}</div>`;
        if (typeof Prism !== 'undefined') {
          Prism.highlightAllUnder($('#artifactContent'));
        }
        if (typeof mermaid !== 'undefined') {
          mermaid.initialize({ startOnLoad: false, theme: 'dark' });
          $$('#artifactContent .language-mermaid').forEach((el, i) => {
             const code = el.textContent;
             const id = 'mermaid-' + Date.now() + '-' + i;
             mermaid.render(id, code).then(res => {
                el.parentElement.outerHTML = `<div class="mermaid-diagram" style="text-align: center; margin: 15px 0;">${res.svg}</div>`;
             }).catch(err => console.error('Mermaid error:', err));
          });
        }
      } else {
        $('#artifactContent').innerHTML = controlsHTML + `<pre style="padding: 1rem; margin: 0; white-space: pre-wrap; font-family: monospace; padding-top: 5px;">${escapeHTML(text)}</pre>`;
      }

      $('#copyArtifactBtn').onclick = () => {
         navigator.clipboard.writeText(text).then(() => toast("Copied artifact source")).catch(() => toast("Failed to copy", true));
      };
      $('#downloadArtifactBtn').onclick = () => {
         const blob = new Blob([text], { type: 'text/plain' });
         const url = URL.createObjectURL(blob);
         const a = document.createElement('a');
         a.href = url;
         a.download = name;
         document.body.appendChild(a);
         a.click();
         document.body.removeChild(a);
         URL.revokeObjectURL(url);
         toast("Downloaded " + name);
      };
    } catch (err) {
      console.error(err);
      $('#artifactContent').innerHTML = '<div class="empty">Failed to load content</div>';
    }
  }

  function escapeHTML(str) {
    if (!str) return '';
    return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  window.openMemoryEditor = function(id, path, text) {
    const kind = path === '__GLOBAL__' ? 'global_concept' : 'structural';
    const anchors = path === '__GLOBAL__' ? [] : [path];
    const el = $('#frame-' + id) || $('#fileContent');
    const html = `
      <div class="memory-editor" id="editor-${id}">
        <textarea id="editor-text-${id}">${escapeHTML(text)}</textarea>
        <div class="memory-editor-actions">
          <button type="button" class="danger" onclick="fetchMemoryFile('${escapeHTML(path)}')">CANCEL</button>
          <button type="button" class="primary" onclick="saveMemoryFrame(${id}, '${escapeHTML(path)}', '${escapeHTML(kind)}')">SAVE</button>
        </div>
      </div>
    `;
    if (id === 0) {
      $('#add-frame-container').innerHTML = html;
    } else {
      el.innerHTML = html;
    }
  };

  window.saveMemoryFrame = async function(id, path, kind) {
    const text = $('#editor-text-' + id).value;
    const anchors = path === '__GLOBAL__' ? [] : [path];
    
    // show saving state
    $('#editor-' + id + ' button.primary').disabled = true;
    $('#editor-' + id + ' button.primary').innerText = 'SAVING...';

    try {
      const res = await fetch('/api/memory/frame', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Loa-Token': token },
        body: JSON.stringify({ id: Number(id), kind, text, anchors })
      });
      if (!res.ok) throw new Error('Failed to save frame');
      fetchMemoryFile(path);
    } catch (err) {
      alert(err.message);
      $('#editor-' + id + ' button.primary').disabled = false;
      $('#editor-' + id + ' button.primary').innerText = 'SAVE';
    }
  };

  function renderMemoryFile(path, data) {
    let title = path === '__GLOBAL__' ? 'GLOBAL KNOWLEDGE' : escapeHTML(path);
    let html = `<div class="memory-detail-section"><h3>${title}</h3></div>`;

    html += `<div class="memory-detail-section"><h3>NARRATIVE FRAMES</h3>`;
    html += `<button class="memory-add-btn" onclick="openMemoryEditor(0, '${escapeHTML(path)}', '')">+ ADD FRAME</button>`;
    html += `<div id="add-frame-container"></div>`;

    if (data.frames && data.frames.length > 0) {
      for (const f of data.frames) {
        html += `<div class="memory-item" id="frame-${f.id}">
          <div class="row">
            <span class="mem-kind">${escapeHTML(f.kind)}</span>
            <span class="timestamp">${escapeHTML(new Date(f.created_at).toLocaleString())}</span>
            <button class="memory-edit-btn" onclick="openMemoryEditor(${f.id}, '${escapeHTML(path)}', ${escapeHTML(JSON.stringify(f.text))})">EDIT</button>
          </div>
          <div class="text">${escapeHTML(f.text)}</div>
        </div>`;
      }
    } else {
      html += `<div class="empty" style="padding: 10px 0; text-align: left;">No narrative frames yet.</div>`;
    }
    html += `</div>`;

    if (data.symbols && data.symbols.length > 0) {
      html += `<div class="memory-detail-section"><h3>AST SYMBOLS</h3>`;
      for (const s of data.symbols) {
        let sig = s.signature ? escapeHTML(s.signature) : '';
        html += `<div class="ast-symbol">
          <div class="ast-symbol-name">${escapeHTML(s.name)} <span style="color:var(--muted)">${sig}</span></div>
          <div class="ast-symbol-meta">${escapeHTML(s.kind)} • Line ${s.line}</div>
        </div>`;
      }
      html += `</div>`;
    }

    if ((!data.frames || data.frames.length === 0) && (!data.symbols || data.symbols.length === 0)) {
      if (path !== '__GLOBAL__') {
         html += `<div class="empty">No AST symbols found for this file.</div>`;
      }
    }

    $('#fileContent').innerHTML = html;
  }

  let currentFetchId = 0;
  let isRefreshing = false;

  async function refresh() {
    if (isRefreshing) return;
    isRefreshing = true;
    const fetchId = ++currentFetchId;
    try {
      let url = '/api/snapshot';
      let qs = [];
      if (lastSnapshotVersion !== null) {
        qs.push('v=' + lastSnapshotVersion);
      }
      qs.push('last_log_id=' + globalExecutionLogLastId);
      qs.push('last_log_session=' + encodeURIComponent(activeSessionID));
      qs.push('_t=' + Date.now());
      url += '?' + qs.join('&');

      if (window.logUIEvent) window.logUIEvent('fetch_start', url);
      const res = await fetch(url);
      
      if (fetchId !== currentFetchId) {
        return; // Another fetch started after us, ignore this response
      }

      if (res.status === 304) {
        if (window.logUIEvent) window.logUIEvent('fetch_304', url);
        return;
      }
      if (!res.ok) throw new Error('snapshot fetch failed');
      const data = await res.json();
      
      if (fetchId !== currentFetchId) {
        return; // Race condition during json parsing
      }
      
      const newSessionID = data.sessions?.active_id || '';
      if (newSessionID !== activeSessionID) {
        globalExecutionLog = [];
        globalExecutionLogLastId = 0;
      }
      if (data.state && data.state.execution_log) {
        globalExecutionLog.push(...data.state.execution_log);
        if (globalExecutionLog.length > 0) {
          globalExecutionLogLastId = globalExecutionLog[globalExecutionLog.length - 1].id;
        }
        data.state.execution_log = globalExecutionLog;
      } else if (data.state) {
        data.state.execution_log = globalExecutionLog;
      }
      
      snapshot = data;
      if (snapshot && snapshot.state && snapshot.state.sequence !== undefined) {
        lastSnapshotVersion = snapshot.state.sequence;
        if (window.logUIEvent) window.logUIEvent('fetch_resolved', snapshot.state.sequence);
      }
      render(snapshot);
    } catch (e) {
      if (fetchId !== currentFetchId) return;
      console.error(e);
      setStatus('OFFLINE', 'fail');
    } finally {
      isRefreshing = false;
    }
  }

  function render(x) {
    const st = x.state || {};
    const rt = x.runtime || {};
    const sessions = x.sessions || {};
    const persistence = x.persistence || {};
    activeSessionID = sessions.active_id || '';

    if (st.complexity_mode) {
      const radio = document.querySelector(`input[name="complexityModeCtrl"][value="${st.complexity_mode}"]`);
      if(radio) radio.checked = true;
    }

    if ($('#readOnlyCtrl') && st.read_only !== undefined) {
      $('#readOnlyCtrl').checked = st.read_only;
    }

    $('#infCount').textContent = rt.stats?.inferences ?? 0;
    $('#embCount').textContent = rt.stats?.embeddings ?? 0;
    renderInferenceBreakdown(rt.stats?.inference_by_primitive || {});

    if (st.pending_question) setStatus('WAITING USER', 'wait');
    else if (pendingApproval(st)) setStatus('WAITING APPROVAL', 'wait');
    else if (rt.running) setStatus(st.active_task?.status?.toUpperCase() || 'THINKING', 'busy');
    else if (st.active_task?.status === 'paused') setStatus('PAUSED', 'wait');
    else if (rt.last_error) setStatus('ERROR', 'fail');
    else setStatus('IDLE', '');

    const pauseBtn = $('button[data-action="pause"]');
    const resumeBtn = $('button[data-action="resume"]');
    const stopBtn = $('button[data-action="stop"]');
    if (pauseBtn && resumeBtn && stopBtn) {
      if (rt.running && st.active_task && st.active_task.status === 'running') {
        pauseBtn.style.display = 'block';
        resumeBtn.style.display = 'none';
        pauseBtn.disabled = false;
        stopBtn.disabled = false;
      } else if (st.active_task && st.active_task.status === 'paused') {
        pauseBtn.style.display = 'none';
        resumeBtn.style.display = 'block';
        resumeBtn.disabled = false;
        stopBtn.disabled = true;
      } else {
        pauseBtn.style.display = 'block';
        pauseBtn.disabled = true;
        resumeBtn.style.display = 'none';
        stopBtn.disabled = true;
      }
    }

    const createBtn = $('#createSessionBtn');
    if (createBtn) {
      if (rt.running) {
        createBtn.disabled = true;
        createBtn.innerText = (rt.reason || 'BUSY').toUpperCase();
      } else {
        createBtn.disabled = false;
        createBtn.innerText = 'CREATE';
      }
    }

    $('#indexState').textContent = rt.index_available
      ? ('INDEX ONLINE' + (rt.index_warning ? ' · ' + rt.index_warning : ''))
      : (rt.index_warning || 'INDEX OFFLINE');

    renderChat(st.messages || []);
    renderComposer(rt, st);
    renderPlan(st.active_task, st.completed_tasks || []);
    renderGraph(st.active_task, st.completed_tasks || []);
    renderContext(rt.last_context || 'No inference context yet.');
    renderWorkingMemory(st.active_task, x.memory || [], rt.last_context || '');
    renderMemory(x.memory || [], x.task_summaries || []);
    renderLog(st.execution_log || []);
    renderUploads(st.uploads || []);
    renderAttachments(st.active_attachments || []);
    renderSessions(sessions, rt, st);
    renderPersistence(persistence, sessions);
    maybeAutosaveToast(persistence);
    maybeApproval(st, sessions.active_id || '');
    fillSettings(x.config, x.loaignore);
    if (rt.needs_project_setup) {
      if (!$('#projectSetupDialog').open) {
        $('#projectSetupDialog').showModal();
        if (!$('#projSetupIgnore').value) {
          $('#projSetupIgnore').value = "node_modules/\n.git/\n.loa/\nloa\nvendor/\ndist/\nbuild/";
        }
      }
    } else {
      ensureSessionDialog(sessions);
    }

    let hasEmbedding = false;
    if (x.config?.embedding_engine === 'local') {
      hasEmbedding = !!x.config?.local_embedding_model_path;
    } else {
      hasEmbedding = !!x.config?.embedding_model;
    }

    if ((!x.config?.model_crawling || !hasEmbedding) && !initialSetupComplete) {
      if (!$('#globalSetupDialog').open) {
        $('#globalSetupDialog').showModal();
        $('#setupUrl').value = x.config?.ollama_url || 'http://127.0.0.1:11434';
      }
    } else {
      $('#closeSettings').style.display = 'block';
    }
  }

  function setStatus(text, cls) {
    $('#statusText').textContent = text;
    $('.status').className = 'status ' + cls;
  }

  function pendingApproval(st) {
    return [...(st.tool_calls || [])].reverse().find(c => c.requires_approval && c.approved == null);
  }

  function renderMarkdown(text) {
    if (!text) return '';
    let html = esc(text);
    const isMd = /```|^\s*#|^\s*\d+\.|^\s*[-*] |\*\*/m.test(text);
    if (!isMd) return html;

    html = html.replace(/```(.*?)\n?([\s\S]*?)```/g, (match, lang, code) => {
      return '<pre class="codeblock" style="margin:8px 0;white-space:pre-wrap;word-break:break-all;"><code>' + code + '</code></pre>';
    });
    html = html.replace(/`([^`\n]+)`/g, '<code style="background:rgba(255,255,255,0.1);padding:2px 4px;border-radius:3px;color:var(--cyan);">$1</code>');
    html = html.replace(/\*\*([^\n]+?)\*\*/g, '<strong style="color:#bdefff;">$1</strong>');
    html = html.replace(/\*([^\n]+?)\*/g, '<em style="color:#a8c5d0;">$1</em>');
    html = html.replace(/^### (.*?)$/gm, '<strong style="font-size:1.1em;color:var(--cyan);">$1</strong>');
    html = html.replace(/^## (.*?)$/gm, '<strong style="font-size:1.2em;color:var(--cyan);">$1</strong>');
    html = html.replace(/^# (.*?)$/gm, '<strong style="font-size:1.3em;color:var(--cyan);">$1</strong>');
    html = html.replace(/^(\s*[-*] )(.*?)$/gm, '<strong style="color:var(--cyan);">$1</strong>$2');
    html = html.replace(/^(\s*\d+\. )(.*?)$/gm, '<strong style="color:var(--cyan);">$1</strong>$2');

    return html;
  }

  let chatMessagesMap = {};
  let selectedPlanId = null;

  function renderChat(messages) {
    const sig = signature(messages.map(m => [m.id, m.source, m.text, m.created_at, !!m.clarification, !!m.intervention]));
    if (sig === renderSignatures.chat) return;
    renderSignatures.chat = sig;
    
    chatMessagesMap = {};
    messages.forEach(m => chatMessagesMap[m.id] = m.text);
    
    const box = $('#chat');
    box.innerHTML = messages.map(m => `<div class="msg ${esc(m.source)} ${m.clarification ? 'clarification' : ''} ${m.intervention ? 'intervention' : ''}"><div class="meta">${esc(m.source).toUpperCase()} · ${esc(dt(m.created_at))}${m.clarification ? ' · CLARIFICATION' : ''}${m.intervention ? ' · GUIDANCE' : ''}</div><div class="bubble">${renderMarkdown(m.text)}<button class="copy-btn" data-id="${m.id}" title="Copy raw message"><svg viewBox="0 0 24 24" width="12" height="12" stroke="currentColor" stroke-width="2" fill="none" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg></button></div></div>`).join('');
    box.scrollTop = box.scrollHeight;
  }

  function renderComposer(rt, st) {
    const input = $('#messageInput');
    const button = $('#chatForm button[type="submit"]');
    
    if (rt.reason) {
      input.placeholder = rt.reason.toUpperCase();
      input.disabled = true;
      button.disabled = true;
      button.textContent = 'WAIT';
      return;
    }
    if (st.active_task && st.active_task.status === 'paused') {
      input.placeholder = 'TASK PAUSED. RESUME TO CONTINUE.';
      input.disabled = true;
      button.disabled = true;
      button.textContent = 'PAUSED';
      return;
    }
    
    input.disabled = false;
    button.disabled = false;

    if (rt.running && st.active_task && !st.pending_question) {
      input.placeholder = 'Send guidance to active task…  Ctrl+Enter to send';
      button.textContent = 'STEER';
      return;
    }
    input.placeholder = st.pending_question ? 'Answer Loa…  Ctrl+Enter to send' : 'Talk to Loa…  Ctrl+Enter to send';
    button.textContent = 'SEND';
  }

  function renderPlan(task, completed) {
    let shown = null;
    if (selectedPlanId) {
      shown = completed.find(t => t.id === selectedPlanId);
      if (!shown && task && task.id === selectedPlanId) shown = task;
    }
    if (!shown) {
      shown = task || completed[completed.length - 1] || null;
      if (selectedPlanId && shown) selectedPlanId = shown.id;
    }

    const toolbarSig = signature([activeSessionID, task?.id, completed.map(t => t.id)]);
    if (toolbarSig !== renderSignatures.planToolbar) {
      renderSignatures.planToolbar = toolbarSig;
      const tb = $('#planToolbar');
      if (completed.length > 0 || task) {
        tb.style.display = 'block';
        const options = [];
        completed.forEach(t => {
          options.push(`<option value="${t.id}" ${shown && shown.id === t.id ? 'selected' : ''}>Task ${t.id}: ${esc(t.title || t.goal)}</option>`);
        });
        if (task) {
          options.push(`<option value="${task.id}" ${shown && shown.id === task.id ? 'selected' : ''}>Task ${task.id}: ${esc(task.title || task.goal)} (Active)</option>`);
        }
        tb.innerHTML = `<select id="planHistorySelect" class="session-select" style="width: 100%; max-width: none;">${options.join('')}</select>`;
        $('#planHistorySelect').onchange = (e) => {
          selectedPlanId = parseInt(e.target.value, 10);
          renderSignatures.plan = '';
          renderSignatures.graph = '';
          render(snapshot);
        };
      } else {
        tb.style.display = 'none';
        tb.innerHTML = '';
      }
    } else {
      const sel = $('#planHistorySelect');
      if (sel && shown) sel.value = shown.id;
    }

    const sig = signature([activeSessionID, globalExecutionLogLastId, shown && {
      id: shown.id,
      goal: shown.goal,
      status: shown.status,
      failure: shown.failure,
      acceptance_criteria: shown.acceptance_criteria,
      acceptance_checks: shown.acceptance_checks,
      plan: shown.plan
    }]);
    if (sig === renderSignatures.plan) return;
    renderSignatures.plan = sig;
    const el = $('#plan');
    if (!shown) {
      el.innerHTML = '<div class="empty">No active plan.</div>';
      const artifactList = $('#artifactList');
      if (artifactList) artifactList.innerHTML = '<div class="empty">No active task</div>';
      return;
    }
    
    renderArtifactList(snapshot.artifacts);

    const label = task ? 'ACTIVE TASK' : 'LAST TASK';
    const authority = task ? ` · ${shown.may_modify ? 'MODIFY ALLOWED' : 'READ ONLY'}` : '';
    
    let approvalHTML = '';
    if (shown.status === 'awaiting_approval') {
      approvalHTML = `<div class="approval-gate" style="margin-top: 15px; padding-top: 15px; border-top: 1px solid var(--border);">
        <button class="primary" id="approvePlanBtn" style="padding: 8px 16px; margin-right: 10px; font-weight: bold;">APPROVE PLAN</button>
        <button class="danger" id="rejectPlanBtn" style="padding: 8px 16px; font-weight: bold;">REJECT PLAN</button>
      </div>`;
    }

    el.innerHTML = `<div class="task-head"><b>${label}</b><div class="task-title-ui">${esc(shown.title || shown.goal)}</div><small class="muted">${esc(shown.status)} · plan v${shown.plan?.version || 1}${authority}</small></div>${failureHTML(shown.failure)}${criteriaHTML(shown)}${stepsHTML(shown)}${approvalHTML}`;
    bindExpandables(el);

    if (shown.status === 'awaiting_approval') {
      const approveBtn = $('#approvePlanBtn');
      const rejectBtn = $('#rejectPlanBtn');
      if (approveBtn) {
        approveBtn.addEventListener('click', () => {
          approveBtn.disabled = true;
          if (rejectBtn) rejectBtn.disabled = true;
          api('/api/plan/approval', { method: 'POST', body: JSON.stringify({ approved: true }) }).catch(err => alert("Error approving plan: " + err.message));
        });
      }
      if (rejectBtn) {
        rejectBtn.addEventListener('click', () => {
          rejectBtn.disabled = true;
          if (approveBtn) approveBtn.disabled = true;
          api('/api/plan/approval', { method: 'POST', body: JSON.stringify({ approved: false }) }).catch(err => alert("Error rejecting plan: " + err.message));
        });
      }
    }
  }

  let network = null;
  let currentGraphSteps = null;

  function renderGraph(task, completed) {
    let shown = null;
    if (selectedPlanId) {
      shown = completed.find(t => t.id === selectedPlanId);
      if (!shown && task && task.id === selectedPlanId) shown = task;
    }
    if (!shown) {
      shown = task || completed[completed.length - 1] || null;
    }
    
    if (!shown || !shown.plan || !shown.plan.steps) {
        if (network) { network.destroy(); network = null; }
        const container = $('#planGraph');
        if (container) container.innerHTML = '<div class="empty" style="color: var(--muted); padding: 20px; text-align: center;">No active plan to graph.</div>';
        renderSignatures.graph = '';
        currentGraphSteps = null;
        return;
    }
    
    const sig = signature([activeSessionID, shown.plan.steps]);
    if (sig === renderSignatures.graph) return;
    renderSignatures.graph = sig;
    currentGraphSteps = shown.plan.steps;

    const container = $('#planGraph');
    if (!container) return;

    if (!window.vis) return; // vis-network not loaded yet

    if (!network && container.querySelector('.empty')) {
        container.innerHTML = '';
    }

    const nodes = [];
    const edges = [];
    
    shown.plan.steps.forEach((s, i) => {
        let color = '#0a1720';
        let fontColor = '#cfeaf5';
        let border = '#153348';
        if (s.status === 'completed') { color = '#091814'; border = '#215646'; fontColor = '#63ffca'; }
        else if (s.status === 'running') { color = '#071b25'; border = '#1aaee8'; fontColor = '#63d8ff'; }
        else if (s.status === 'failed') { color = '#1a0d12'; border = '#61283a'; fontColor = '#ff607d'; }
        
        nodes.push({
            id: s.id,
            label: `[${i+1}] ${s.title.substring(0, 30)}${s.title.length > 30 ? '...' : ''}`,
            title: s.goal,
            color: { background: color, border: border, highlight: { background: color, border: border } },
            font: { color: fontColor, face: 'ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace', size: 12 },
            shape: 'box',
            borderWidth: 1,
            margin: { top: 8, bottom: 8, left: 10, right: 10 }
        });

        if (s.depends_on) {
            s.depends_on.forEach(dep => {
                edges.push({
                    from: dep,
                    to: s.id,
                    color: { color: '#153348' },
                    arrows: 'to'
                });
            });
        }
    });

    const data = { nodes: new vis.DataSet(nodes), edges: new vis.DataSet(edges) };
    const options = {
        layout: { hierarchical: { direction: 'UD', sortMethod: 'directed', nodeSpacing: 200, levelSeparation: 100 } },
        physics: false,
        interaction: { hover: true, dragNodes: true }
    };

    if (!network) {
        network = new vis.Network(container, data, options);
        network.on('click', function (params) {
            if (params.nodes.length > 0 && currentGraphSteps) {
                const nodeId = params.nodes[0];
                const step = currentGraphSteps.find(s => s.id === nodeId);
                if (step) {
                    showStepDetails(step);
                }
            }
        });
    } else {
        network.setData(data);
    }
  }

  function showStepDetails(step) {
      const html = `<div class="step-detail-view">
          <h4 style="margin-top:0; color: var(--cyan);">${esc(step.title)}</h4>
          <p><strong>Goal:</strong> ${esc(step.goal)}</p>
          <p><strong>Status:</strong> <span class="badge" style="background:var(--bg); border:1px solid var(--border);">${esc(step.status)}</span></p>
          <p><strong>Mode:</strong> ${esc(step.mode)}</p>
          ${step.depends_on && step.depends_on.length ? `<p><strong>Dependencies:</strong> ${step.depends_on.join(', ')}</p>` : ''}
          ${step.failure ? `<p style="color:var(--bad);"><strong>Failure:</strong> ${esc(step.failure)}</p>` : ''}
      </div>`;
      $('#stepDetailsContent').innerHTML = html;
      $('#stepDetailsDialog').showModal();
  }


  function failureHTML(f) {
    if (!f) return '';
    return `<div class="failure-box"><b>LAST HARD FAILURE</b><div>${esc(f.reason)}</div><small>${f.failed_step_id ? `failed step ${esc(f.failed_step_id)} · ` : ''}${f.last_successful_step_id ? `last success ${esc(f.last_successful_step_id)} · ` : ''}${f.recoverable ? 'resume available' : 'not recoverable'}</small></div>`;
  }

  function detailsAttrs(key) {
    return `data-expand-key="${esc(key)}"${expandedItems.has(key) ? ' open' : ''}`;
  }

  function criteriaHTML(task) {
    const criteria = task.acceptance_criteria || [];
    if (!criteria.length) return '';
    const checks = new Map((task.acceptance_checks || []).map(c => [c.criterion_id, c]));
    return `<div class="criteria"><div class="section-label">ACCEPTANCE CRITERIA</div>${criteria.map(c => {
      const x = checks.get(c.id);
      const status = x?.status || 'unverified';
      const details = [...(x?.evidence || []), ...(x?.missing ? [`Missing: ${x.missing}`] : [])];
      const key = `criterion:${activeSessionID}:${task.id}:${c.id}`;
      return `<details class="criterion ${esc(status)}" ${detailsAttrs(key)}><summary><span>${esc(c.id)}.</span><div>${esc(c.requirement)}</div><span class="criterion-status">${esc(status)}</span></summary>${details.length ? `<div class="criterion-evidence">${details.map(v => `<div>${esc(v)}</div>`).join('')}</div>` : ''}</details>`;
    }).join('')}</div>`;
  }

  function stepsHTML(shown) {
    const steps = shown.plan?.steps || [];
    const stepCounts = {};
    if (globalExecutionLog) {
      for (const log of globalExecutionLog) {
        if (log.task_id === shown.id && log.step_id !== undefined) {
          stepCounts[log.step_id] = (stepCounts[log.step_id] || 0) + 1;
        }
      }
    }
    return steps.map((s, i) => {
      const count = stepCounts[s.id] || 0;
      const countHTML = count > 0 ? `<div style="font-size:9px; color:var(--muted); margin-top:4px; text-align:center; font-weight:bold;">${count}x</div>` : '';
      return `<div class="step ${esc(s.status)}"><div style="display:flex; flex-direction:column; align-items:center;"><div class="num">${s.status === 'completed' ? '✓' : s.status === 'running' ? '▶' : s.status === 'failed' ? '!' : String(i + 1).padStart(2, '0')}</div>${countHTML}</div><div><div class="title">${esc(s.title)}</div><div class="goal">${esc(s.goal)}</div></div><span class="badge">${esc(s.mode)}</span></div>`;
    }).join('') || '<div class="empty">No plan steps.</div>';
  }

  function bindExpandables(container) {
    container.querySelectorAll('details[data-expand-key]').forEach(details => {
      details.addEventListener('toggle', () => {
        const key = details.dataset.expandKey;
        if (!key) return;
        if (details.open) expandedItems.add(key);
        else expandedItems.delete(key);
      });
    });
  }

  function renderContext(text) {
    if (text === renderSignatures.context) return;
    renderSignatures.context = text;
    
    let formattedText = esc(text || 'No inference context yet.');
    formattedText = formattedText.replace(/```([a-z]*)\n([\s\S]*?)```/g, (match, lang, code) => {
      let color = '#a9c8d5';
      if (lang === 'json') color = '#e6db74';
      if (lang === 'go') color = '#a6e22e';
      if (lang === 'js' || lang === 'javascript') color = '#66d9ef';
      if (lang === 'html' || lang === 'css') color = '#f92672';
      return `<pre class="codeblock" style="background:#091821; border:1px solid #0c2634; margin:8px 0; padding:12px; border-radius:4px;"><code style="color:${color};">${code}</code></pre>`;
    });

    const out = $('#context');
    if (out) out.innerHTML = formattedText;

    const budget = snapshot?.config?.context_budget || 100000;
    const tokens = Math.floor((text || '').length / 4);
    const meterFill = $('#budgetMeterFill');
    const meterText = $('#budgetMeterText');
    if (meterFill && meterText) {
      const pct = Math.min(100, Math.max(0, (tokens / budget) * 100));
      meterFill.style.width = pct + '%';
      meterFill.style.background = pct > 90 ? 'var(--bad)' : (pct > 75 ? 'var(--warn)' : 'var(--cyan)');
      meterText.textContent = `${tokens.toLocaleString()} / ${budget.toLocaleString()} tokens (est.)`;
    }
  }

  async function purgeWorkingMemory(type, val) {
    if (!confirm('Are you sure you want to purge this from memory? The agent will lose this context on retry.')) return;
    try {
      const payload = { type };
      if (type === 'extra') payload.index = val;
      if (type === 'session_memory') payload.id = val;
      const res = await fetch('/api/memory/purge', {
        method: 'POST',
        headers: {'Content-Type': 'application/json'},
        body: JSON.stringify(payload)
      });
      if (!res.ok) throw new Error(await res.text());
    } catch (e) {
      alert('Failed to purge memory: ' + e.message);
    }
  }

  function renderWorkingMemory(activeTask, memory, ctxText) {
    const cont = $('#workingMemoryContainer');
    if (!cont) return;
    
    const sig = signature([activeTask?.extras, memory, ctxText?.length]);
    if (sig === renderSignatures.workingMemory) return;
    renderSignatures.workingMemory = sig;

    let html = '';
    const ctxLen = ctxText ? ctxText.length : 0;
    html += `<div style="margin-bottom: 20px;">
      <h4 style="margin: 0 0 5px 0;">Task Context Size</h4>
      <div style="font-family: monospace; color: var(--cyan);">${ctxLen.toLocaleString()} chars</div>
    </div>`;

    html += `<h4 style="margin: 0 0 10px 0;">Staged Extras (Current Step)</h4>`;
    const extras = activeTask?.extras || [];
    if (extras.length === 0) {
      html += `<div style="font-size: 12px; color: var(--muted); margin-bottom: 20px;">No extras staged.</div>`;
    } else {
      extras.forEach((ext, i) => {
        let preview = ext.substring(0, 150);
        if (ext.length > 150) preview += '...';
        html += `<div class="memory-card" style="display: flex; justify-content: space-between; align-items: flex-start; padding: 10px; background: var(--bg); border: 1px solid var(--border); border-radius: 4px; margin-bottom: 10px;">
          <div style="font-family: monospace; font-size: 11px; white-space: pre-wrap; word-break: break-all; flex: 1;">${escapeHTML(preview)}</div>
          <button onclick="purgeWorkingMemory('extra', ${i})" class="danger" style="padding: 2px 5px; font-size: 10px; margin-left: 10px;" title="Purge this Extra">🗑</button>
        </div>`;
      });
    }

    html += `<h4 style="margin: 20px 0 10px 0;">Staged Session Memories</h4>`;
    const sm = memory || [];
    if (sm.length === 0) {
      html += `<div style="font-size: 12px; color: var(--muted);">No staged session memories loaded.</div>`;
    } else {
      sm.forEach((m) => {
        let preview = m.text.substring(0, 150);
        if (m.text.length > 150) preview += '...';
        html += `<div class="memory-card" style="display: flex; justify-content: space-between; align-items: flex-start; padding: 10px; background: var(--bg); border: 1px solid var(--border); border-radius: 4px; margin-bottom: 10px;">
          <div>
            <div style="font-size: 10px; font-weight: bold; color: var(--muted); margin-bottom: 5px;">${escapeHTML(m.kind.toUpperCase())} (ID: ${m.id})</div>
            <div style="font-family: monospace; font-size: 11px; white-space: pre-wrap; word-break: break-all; flex: 1;">${escapeHTML(preview)}</div>
          </div>
          <button onclick="purgeWorkingMemory('session_memory', ${m.id})" class="danger" style="padding: 2px 5px; font-size: 10px; margin-left: 10px;" title="Purge this Memory">🗑</button>
        </div>`;
      });
    }
    
    cont.innerHTML = html;
  }


  let currentMemoryItems = [];

  function renderMemory(mem, sums) {
    currentMemoryItems = [...sums.map(s => ({kind:'task_summary', text:s.summary, created_at:s.created_at})), ...mem]
      .sort((a, b) => new Date(b.created_at) - new Date(a.created_at));
    const sig = signature(currentMemoryItems.map(m => [m.id, m.kind, m.text, m.created_at]));
    if (sig === renderSignatures.memory) return;
    renderSignatures.memory = sig;
    applyMemoryFilter();
  }

  function applyMemoryFilter() {
    const q = ($('#memorySearchInput')?.value || '').toLowerCase();
    const filtered = currentMemoryItems.filter(m => !q || (m.text || '').toLowerCase().includes(q) || (m.kind || '').toLowerCase().includes(q));
    
    $('#memory').innerHTML = filtered.map(m => {
      const actions = m.id ? `<div class="mem-actions" style="margin-left: auto;">
        <button type="button" class="icon-btn edit-mem-btn" data-id="${m.id}" title="Edit Memory" style="background:transparent; border:none; color:var(--cyan); cursor:pointer;">✎</button>
        <button type="button" class="icon-btn delete-mem-btn" data-id="${m.id}" title="Delete Memory" style="background:transparent; border:none; color:var(--bad); cursor:pointer;">×</button>
      </div>` : '';
      return `<div class="memory-item"><div class="row"><span class="mem-kind">${esc(m.kind)}</span><span class="timestamp">${esc(dt(m.created_at))}</span>${actions}</div><div class="text">${esc(m.text)}</div></div>`;
    }).join('') || '<div class="empty">No memories found.</div>';
    
    $$('.edit-mem-btn').forEach(btn => btn.addEventListener('click', (e) => {
      const id = parseInt(e.currentTarget.dataset.id);
      const mem = currentMemoryItems.find(x => x.id === id);
      if (!mem) return;
      $('#memoryEditDialog').dataset.id = id;
      $('#memoryEditTextInput').value = mem.text;
      $('#memoryEditDialog').showModal();
    }));
    
    $$('.delete-mem-btn').forEach(btn => btn.addEventListener('click', async (e) => {
      if (!confirm('Are you sure you want to delete this memory?')) return;
      const id = parseInt(e.currentTarget.dataset.id);
      try {
        await api('/api/memory/delete', { method: 'POST', body: JSON.stringify({ id }) });
        toast('Memory deleted');
        refresh();
      } catch (err) {
        toast('Failed to delete: ' + err.message, true);
      }
    }));
  }

  function renderLogFilters() {
    const container = $('#logFilters');
    if (!container) return;
    container.innerHTML = logCategories.map(c => 
      `<div class="log-filter ${activeLogFilters.has(c) ? 'active' : ''}" data-category="${c}">${c}</div>`
    ).join('');
    container.querySelectorAll('.log-filter').forEach(el => {
      el.addEventListener('click', (e) => {
        const cat = e.target.getAttribute('data-category');
        if (activeLogFilters.has(cat)) activeLogFilters.delete(cat);
        else activeLogFilters.add(cat);
        renderLogFilters();
        renderSignatures.log = '';
        if (snapshot) renderLog(snapshot.state.execution_log || []);
      });
    });
  }

  function formatLogBlock(text) {
    if (!text) return '';
    if (typeof marked !== 'undefined') {
      return `<div class="markdown-body" style="padding: 10px; background: #07151e; border-radius: 4px; margin-top: 5px;">${marked.parse(text)}</div>`;
    }
    return `<pre class="codeblock">${esc(text)}</pre>`;
  }

  window.openLogDetail = function(id) {
    const log = globalExecutionLog.find(x => x.id === id);
    if (!log) return;
    
    $('#logDetailTitle').textContent = log.primitive || log.kind || 'EXECUTION LOG';
    $('#logDetailTime').textContent = dt(log.created_at);
    
    if (log.input) {
      $('#logDetailInputContainer').style.display = 'block';
      $('#logDetailInput').innerHTML = formatLogBlock(log.input);
    } else {
      $('#logDetailInputContainer').style.display = 'none';
      $('#logDetailInput').innerHTML = '';
    }
    
    if (log.raw_output) {
      $('#logDetailOutputContainer').style.display = 'block';
      $('#logDetailOutput').innerHTML = formatLogBlock(log.raw_output);
    } else {
      $('#logDetailOutputContainer').style.display = 'none';
      $('#logDetailOutput').innerHTML = '';
    }
    
    const modal = $('#logDetailModal');
    modal.showModal();
    
    if (typeof Prism !== 'undefined') {
      Prism.highlightAllUnder(modal);
    }
  };

  function renderLog(log) {
    const filtered = log.filter(x => activeLogFilters.has(x.category || 'system'));
    const sigKey = Array.from(activeLogFilters).sort().join(',');
    const last = filtered[filtered.length - 1];
    const sig = `${activeSessionID}:${sigKey}:${filtered.length}:${last?.id || 0}:${last?.summary || ''}`;
    if (sig === renderSignatures.log) return;
    renderSignatures.log = sig;
    const el = $('#executionLog');
    el.innerHTML = [...filtered].reverse().map(x => {
      return `<div class="log-entry" style="cursor:pointer;" onclick="window.openLogDetail(${x.id})"><div class="row"><span class="primitive">${esc(x.primitive || x.kind)}</span><span class="summary">${esc(x.summary)}</span><span class="timestamp">${esc(dt(x.created_at))}</span></div></div>`;
    }).join('') || '<div class="empty">No execution log entries.</div>';
  }

  function renderSessions(sessions, rt, st) {
    const list = sessions.sessions || [];
    const sig = signature([sessions.active_id, list]);
    const sel = $('#sessionSelect');
    if (sig !== renderSignatures.sessions) {
      renderSignatures.sessions = sig;
      sel.innerHTML = list.length
        ? list.map(s => `<option value="${esc(s.id)}">${esc(s.title)}</option>`).join('')
        : '<option value="">NO SESSION</option>';
      sel.value = sessions.active_id || '';
    } else if (sel.value !== (sessions.active_id || '')) {
      sel.value = sessions.active_id || '';
    }
    const locked = !!rt.running || !!pendingApproval(st);
    sel.disabled = locked || list.length < 2;
    $('#newSessionBtn').disabled = locked;
  }

  function renderPersistence(p, sessions) {
    const btn = $('#saveBtn');
    btn.classList.toggle('dirty', !!p.dirty);
    btn.disabled = !sessions.active_id;
    const when = p.last_saved_at ? dateTime(p.last_saved_at) : '';
    btn.title = when ? `Last saved: ${when}${p.dirty ? '\nUnsaved changes' : ''}` : 'Not saved yet';
  }

  function maybeAutosaveToast(p) {
    const seq = Number(p.save_sequence || 0);
    if (lastSaveSequence === null) {
      lastSaveSequence = seq;
      return;
    }
    if (seq === lastSaveSequence) return;
    lastSaveSequence = seq;
    if (p.last_save_reason === 'autosave') toast('State autosaved');
  }

  function ensureSessionDialog(sessions) {
    if (!sessions.needs_creation) return;
    const dialog = $('#sessionDialog');
    requiredSessionDialog = true;
    $('#closeSession').hidden = true;
    if (!dialog.open) {
      $('#sessionTitleInput').value = '';
      dialog.showModal();
      setTimeout(() => $('#sessionTitleInput').focus(), 0);
    }
  }

  function openSessionDialog(required = false) {
    requiredSessionDialog = required;
    $('#closeSession').hidden = required;
    $('#sessionTitleInput').value = '';
    const dialog = $('#sessionDialog');
    if (!dialog.open) dialog.showModal();
    setTimeout(() => $('#sessionTitleInput').focus(), 0);
  }

  function maybeApproval(st, sessionID) {
    const call = pendingApproval(st);
    if (!call) {
      lastApprovalKey = '';
      if ($('#approvalDialog').open) $('#approvalDialog').close();
      return;
    }
    const key = `${sessionID}:${call.id}`;
    if (key === lastApprovalKey) return;
    lastApprovalKey = key;
    $('#approvalTool').textContent = call.kind;
    $('#approvalReason').textContent = call.description || 'No reason provided';
    $('#approvalTarget').textContent = approvalSubject(call);
    $('#approvalInput').textContent = pretty(call.input);
    $('#approvalGuidanceInput').value = '';
    const allowRun = $('#allowRunBtn');
    const command = call.kind === 'execute_shell' || call.kind === 'execute_process';
    allowRun.hidden = !command || !call.allow_run_eligible;
    $('#approvalDialog').showModal();
    $('#allowBtn').onclick = () => resolveApproval(call.id, true, false);
    $('#denyBtn').onclick = () => resolveApproval(call.id, false, false);
    allowRun.onclick = () => resolveApproval(call.id, true, true);
  }

  function approvalSubject(call) {
    const i = call.input || {};
    if (call.kind === 'execute_shell') return i.command || '';
    if (call.kind === 'execute_process') return [i.binary, ...(i.args || [])].join(' ');
    return i.path || i.query || '(see exact request)';
  }

  async function resolveApproval(id, approved, allowRun) {
    try {
      await api('/api/approval', {method:'POST', body:JSON.stringify({tool_call_id:id, approved, allow_run:allowRun})});
      $('#approvalDialog').close();
      toast(allowRun ? 'Exact command allowed for this run' : approved ? 'Action allowed' : 'Action denied');
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function sendApprovalGuidance() {
    const input = $('#approvalGuidanceInput');
    const text = input.value.trim();
    if (!text) return;
    try {
      await api('/api/message', {method:'POST', body:JSON.stringify({text})});
      input.value = '';
      if ($('#approvalDialog').open) $('#approvalDialog').close();
      toast('Guidance queued; pending action superseded');
      await refresh();
    } catch (e) {
      toast(e.message, true);
    }
  }

  $('#approvalGuidanceBtn').addEventListener('click', sendApprovalGuidance);
  $('#approvalGuidanceInput').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.ctrlKey) {
      e.preventDefault();
      sendApprovalGuidance();
    }
  });

  function renderInferenceBreakdown(counts) {
    const sig = signature(counts);
    if (sig === renderSignatures.inference) return;
    renderSignatures.inference = sig;

    const groupCounts = Object.fromEntries(inferenceGroupOrder.map(k => [k, 0]));
    let other = 0;
    Object.entries(counts).forEach(([primitive, count]) => {
      const group = inferenceGroups[primitive];
      if (group) groupCounts[group] += Number(count || 0);
      else other += Number(count || 0);
    });
    const groupRows = inferenceGroupOrder.map(group => metricRow(group, groupCounts[group] || 0));
    if (other) groupRows.push(metricRow('Other', other));
    $('#inferenceGroups').innerHTML = groupRows.join('');

    const primitiveRows = Object.entries(counts)
      .sort((a, b) => Number(b[1]) - Number(a[1]) || a[0].localeCompare(b[0]))
      .map(([name, count]) => metricRow(name, count));
    $('#inferencePrimitives').innerHTML = primitiveRows.join('') || '<div class="tooltip-empty">No inference calls yet.</div>';
  }

  function metricRow(label, value) {
    return `<div class="metric-row"><span>${esc(label)}</span><b>${esc(value)}</b></div>`;
  }

  function setInferenceTab(tab) {
    inferenceTab = tab === 'primitives' ? 'primitives' : 'groups';
    $$('[data-inference-tab]').forEach(b => b.classList.toggle('active', b.dataset.inferenceTab === inferenceTab));
    $('#inferenceGroups').classList.toggle('active', inferenceTab === 'groups');
    $('#inferencePrimitives').classList.toggle('active', inferenceTab === 'primitives');
  }

  function initInferenceTooltip() {
    $$('[data-inference-tab]').forEach(b => b.addEventListener('click', e => {
      e.stopPropagation();
      setInferenceTab(b.dataset.inferenceTab);
    }));
    $('#inferenceCounter').addEventListener('mouseenter', () => setInferenceTab('groups'));
    $('#inferenceCounter').addEventListener('focusin', e => {
      if (e.target === $('#inferenceCounter')) setInferenceTab('groups');
    });
  }

  function initResizers() {
    const workspace = $('.workspace');
    const chatPane = $('.chat-pane');
    const mainDivider = $('#mainDivider');
    const chatDivider = $('#chatDivider');

    const applyMainWidth = width => {
      if (window.innerWidth <= 900) return width;
      const rect = workspace.getBoundingClientRect();
      const max = Math.max(320, rect.width - 360 - mainDivider.offsetWidth);
      const value = clamp(width, 320, max);
      workspace.style.setProperty('--left-pane-width', `${value}px`);
      return value;
    };
    const applyComposerHeight = height => {
      const rect = chatPane.getBoundingClientRect();
      const max = Math.max(90, rect.height - 34 - 120 - chatDivider.offsetHeight);
      const value = clamp(height, 90, max);
      chatPane.style.setProperty('--composer-height', `${value}px`);
      return value;
    };

    const storedMain = Number(localStorage.getItem('loa.mainPaneWidth'));
    const storedComposer = Number(localStorage.getItem('loa.composerHeight'));
    if (Number.isFinite(storedMain) && storedMain > 0) applyMainWidth(storedMain);
    if (Number.isFinite(storedComposer) && storedComposer > 0) applyComposerHeight(storedComposer);

    wireResizer(mainDivider, e => {
      if (window.innerWidth <= 900) return null;
      const rect = workspace.getBoundingClientRect();
      const width = applyMainWidth(e.clientX - rect.left);
      return () => localStorage.setItem('loa.mainPaneWidth', String(Math.round(width)));
    });

    wireResizer(chatDivider, e => {
      const rect = chatPane.getBoundingClientRect();
      const height = applyComposerHeight(rect.bottom - e.clientY);
      return () => localStorage.setItem('loa.composerHeight', String(Math.round(height)));
    });

    window.addEventListener('resize', () => {
      const currentMain = parseFloat(getComputedStyle(workspace).getPropertyValue('--left-pane-width'));
      const currentComposer = parseFloat(getComputedStyle(chatPane).getPropertyValue('--composer-height'));
      if (Number.isFinite(currentMain)) applyMainWidth(currentMain);
      if (Number.isFinite(currentComposer)) applyComposerHeight(currentComposer);
    });
  }

  function wireResizer(handle, onMove) {
    let active = false;
    let persist = null;
    handle.addEventListener('pointerdown', e => {
      if (e.button !== 0) return;
      active = true;
      document.body.classList.add('resizing');
      handle.setPointerCapture(e.pointerId);
      persist = onMove(e) || persist;
      e.preventDefault();
    });
    handle.addEventListener('pointermove', e => {
      if (!active) return;
      persist = onMove(e) || persist;
    });
    const done = e => {
      if (!active) return;
      active = false;
      document.body.classList.remove('resizing');
      if (persist) persist();
      persist = null;
      try { handle.releasePointerCapture(e.pointerId); } catch {}
    };
    handle.addEventListener('pointerup', done);
    handle.addEventListener('pointercancel', done);
  }

  const clamp = (n, min, max) => Math.max(min, Math.min(max, n));

  $('#chatForm').addEventListener('submit', async e => {
    e.preventDefault();
    const t = $('#messageInput').value.trim();
    if (!t) return;
    try {
      await api('/api/message', {method:'POST', body:JSON.stringify({text:t})});
      $('#messageInput').value = '';
      await refresh();
    } catch (err) {
      toast(err.message, true);
    }
  });

  $('#messageInput').addEventListener('keydown', e => {
    if (e.key === 'Enter' && e.ctrlKey) {
      e.preventDefault();
      $('#chatForm').requestSubmit();
    }
  });

  $$('.tab:not(.settings-tab)').forEach(b => b.addEventListener('click', () => {
    $$('.tab:not(.settings-tab)').forEach(x => x.classList.remove('active'));
    $$('.tabbody:not(.settings-tabbody)').forEach(x => x.classList.remove('active'));
    b.classList.add('active');
    $('#tab-' + b.dataset.tab).classList.add('active');

    if (b.dataset.tab === 'memory-viewer' && !memoryTreeFetched) {
      memoryTreeFetched = true;
      fetchMemoryTree();
    }
  }));

  $$('.top-tab').forEach(b => b.addEventListener('click', () => {
    $$('.top-tab').forEach(x => x.classList.remove('active'));
    b.classList.add('active');
    
    $$('.sub-tabs').forEach(x => x.style.display = 'none');
    
    const group = b.dataset.group;
    const targetSubTabs = $('#subtabs-' + group);
    if (targetSubTabs) {
      targetSubTabs.style.display = 'flex';
      const firstTab = targetSubTabs.querySelector('.tab');
      if (firstTab) {
        firstTab.click();
      }
    }
  }));

  $$('.settings-tab').forEach(b => b.addEventListener('click', () => {
    $$('.settings-tab').forEach(x => x.classList.remove('active'));
    $$('.settings-tabbody').forEach(x => x.classList.remove('active'));
    b.classList.add('active');
    $('#' + b.dataset.target).classList.add('active');
  }));

  $$('[data-action]').forEach(b => b.addEventListener('click', () => {
    if (b.disabled) return;
    action(b.dataset.action);
    $('#dropdownMenu')?.classList.remove('show');
  }));

  const menuBtn = $('#menuBtn');
  if (menuBtn) {
    menuBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      $('#dropdownMenu').classList.toggle('show');
    });
  }
  document.addEventListener('click', () => {
    $('#dropdownMenu')?.classList.remove('show');
  });

  async function action(name) {
    try {
      if (name === 'settings') {
        await openSettings();
        return;
      }
      if (name === 'stop') {
        await api('/api/stop', {method:'POST'});
        toast('Stop requested');
        return;
      }
      if (name === 'pause') {
        await api('/api/pause', {method:'POST'});
        toast('Pause requested');
        return;
      }
      if (name === 'resume') {
        await api('/api/resume', {method:'POST'});
        toast('Resume requested');
        return;
      }
      if (name === 'quit') {
        await api('/api/quit', {method:'POST'});
        toast('Saving and shutting down...');
        setTimeout(() => window.close(), 1000);
        return;
      }
      if (name === 'about') {
        $('#aboutVersion').textContent = appVersion;
        $('#aboutDialog').showModal();
        return;
      }
      if (name === 'save') {
        const r = await api('/api/save', {method:'POST'});
        lastSaveSequence = Number(r.persistence?.save_sequence || lastSaveSequence || 0);
        toast(`Saved ${r.session || 'session'}`);
        await refresh();
        return;
      }
      if (name === 'reindex') {
        const r = await api('/api/reindex', {method:'POST'});
        toast(`Index rebuilt: ${r.files} files`);
        return;
      }
      if (name === 'dump') {
        const r = await api('/api/debug-dump', {method:'POST', body:JSON.stringify({include_embeddings:false, frontend_logs: window.uiExecutionLog})});
        toast(`Debug dump: ${r.file}`);
        return;
      }
    } catch (e) {
      toast(e.message, true);
    }
  }

  $('#approvalDialog').addEventListener('cancel', e => e.preventDefault());

  $('#newSessionBtn').addEventListener('click', () => openSessionDialog(false));
  $('#closeSession').addEventListener('click', () => {
    if (!requiredSessionDialog) $('#sessionDialog').close();
  });
  $('#sessionDialog').addEventListener('cancel', e => {
    if (requiredSessionDialog) e.preventDefault();
  });
  $('#sessionForm').addEventListener('submit', async e => {
    e.preventDefault();
    const title = $('#sessionTitleInput').value.trim();
    if (!title) return;
    try {
      const r = await api('/api/session', {method:'POST', body:JSON.stringify({title})});
      requiredSessionDialog = false;
      $('#sessionDialog').close();
      lastApprovalKey = '';
      lastSaveSequence = Number(r.persistence?.save_sequence || 0);
      try {
        await api('/api/session/modes', {
          method: 'POST',
          body: JSON.stringify({
            complexity_mode: document.querySelector('input[name="complexityModeCtrl"]:checked')?.value,
            read_only: $('#readOnlyCtrl').checked
          })
        });
      } catch (e) {
        console.warn('Failed to sync initial session modes', e);
      }
      toast(`${r.saved_current ? 'Saved current · ' : ''}Session created: ${r.session.title}`);
      await refresh();
    } catch (err) {
      toast(err.message, true);
    }
  });

  $('#sessionSelect').addEventListener('change', async e => {
    const id = e.target.value;
    const current = snapshot?.sessions?.active_id || '';
    if (!id || id === current) return;
    try {
      const r = await api('/api/session/switch', {method:'POST', body:JSON.stringify({id})});
      lastApprovalKey = '';
      lastSaveSequence = Number(r.persistence?.save_sequence || 0);
      try {
        await api('/api/session/modes', {
          method: 'POST',
          body: JSON.stringify({
            complexity_mode: document.querySelector('input[name="complexityModeCtrl"]:checked')?.value,
            read_only: $('#readOnlyCtrl').checked
          })
        });
      } catch (e) {
        console.warn('Failed to sync session modes on switch', e);
      }
      toast(`${r.saved_current ? 'Saved current · ' : ''}Switched to ${r.session.title}`);
      await refresh();
    } catch (err) {
      e.target.value = current;
      toast(err.message, true);
    }
  });

  async function openSettings() {
    try {
      const [m, localM] = await Promise.all([
        api('/api/models'),
        api('/api/models/local')
      ]);
      populateModels(m.models || []);
      populateLocalModels(localM.models || []);
      fillSettings(snapshot?.config || {}, snapshot?.loaignore || '');
      $('#settingsDialog').showModal();
    } catch (e) {
      toast(`Ollama models: ${e.message}`, true);
      populateModels([]);
      fillSettings(snapshot?.config || {}, snapshot?.loaignore || '');
      $('#settingsDialog').showModal();
    }
  }

  $('#closeSettings').onclick = () => $('#settingsDialog').close();
  $('#closeLogDetail').onclick = () => $('#logDetailModal').close();

  function populateModels(models) {
    const html = ['<option value="">— select —</option>', ...models.map(m => `<option value="${esc(m)}">${esc(m)}</option>`)].join('');
    $('#modelCrawling').innerHTML = html;
    $('#modelConversation').innerHTML = html;
    $('#modelPlanning').innerHTML = html;
    $('#modelExecuting').innerHTML = html;
    $('#embeddingModel').innerHTML = html;
  }

  function populateLocalModels(models) {
    const html = ['<option value="">— select —</option>', ...models.map(m => `<option value="${esc(m)}">${esc(m.split('/').pop())}</option>`)].join('');
    if ($('#localEmbeddingModelPath')) $('#localEmbeddingModelPath').innerHTML = html;
    if ($('#setupLocalEmbeddingModelPath')) $('#setupLocalEmbeddingModelPath').innerHTML = html;
  }

  $('#chat').addEventListener('click', (e) => {
    const btn = e.target.closest('.copy-btn');
    if (btn) {
      const id = btn.dataset.id;
      const text = chatMessagesMap[id];
      if (text) {
        navigator.clipboard.writeText(text).then(() => {
          toast("Copied message");
        }).catch(err => {
          toast("Failed to copy", true);
        });
      }
    }
  });

  $('#setupConnectBtn').onclick = async () => {
    const url = $('#setupUrl').value.trim();
    const key = $('#setupKey').value.trim();
    if (!url) return;
    
    const origText = $('#setupConnectBtn').textContent;
    $('#setupConnectBtn').textContent = 'CONNECTING...';
    $('#setupConnectBtn').disabled = true;
    $('#setupConnectResult').textContent = '';
    
    try {
      const c = readConfig();
      c.ollama_url = url;
      c.api_key = key;
      await api('/api/config?scope=global', {method:'POST', body:JSON.stringify(c)});
      const [m, localM] = await Promise.all([
        api('/api/models'),
        api('/api/models/local')
      ]);
      const models = m.models || [];
      const html = ['<option value="">— select —</option>', ...models.map(m => `<option value="${esc(m)}">${esc(m)}</option>`)].join('');
      $('#setupModelCrawling').innerHTML = html;
      $('#setupModelConversation').innerHTML = html;
      $('#setupModelPlanning').innerHTML = html;
      $('#setupModelExecuting').innerHTML = html;
      $('#setupEmbedding').innerHTML = html;
      
      const localModels = localM.models || [];
      const localHtml = ['<option value="">— select —</option>', ...localModels.map(m => `<option value="${esc(m)}">${esc(m.split('/').pop())}</option>`)].join('');
      if ($('#setupLocalEmbeddingModelPath')) $('#setupLocalEmbeddingModelPath').innerHTML = localHtml;
      toast(`Found ${models.length} models`);
      $('#setupConnectResult').textContent = `✅ Success! Found ${models.length} models.`;
      $('#setupConnectResult').style.color = '#34d399';
    } catch (e) {
      toast(`Connection failed: ${e.message}`, true);
      $('#setupConnectResult').textContent = `❌ Connection failed: ${e.message}`;
      $('#setupConnectResult').style.color = '#f87171';
    } finally {
      $('#setupConnectBtn').textContent = origText;
      $('#setupConnectBtn').disabled = false;
    }
  };

  async function testLLM(url, key, chatModel, embedModel, btnEl, resultEl, embedEngine, localEmbedPath) {
    if (!url || (!chatModel && !embedModel && embedEngine !== 'local')) {
      toast('Please configure URL and at least one model before testing', true);
      return;
    }
    if (!chatModel) {
      toast('Please select an inference model to test', true);
      return;
    }
    const origText = btnEl.textContent;
    btnEl.textContent = 'TESTING...';
    btnEl.disabled = true;
    resultEl.textContent = '';
    resultEl.style.color = 'var(--text-bright)';
    
    try {
      const res = await fetch('/api/test-llm', {
        method: 'POST',
        headers: {'Content-Type': 'application/json', 'X-Loa-Token': token},
        body: JSON.stringify({
          ollama_url: url,
          api_key: key,
          chat_model: chatModel,
          embedding_model: embedModel,
          embedding_engine: embedEngine,
          local_embedding_model_path: localEmbedPath
        })
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || 'Test failed');
      }
      resultEl.innerHTML = '<span style="display:inline-block; padding:4px 8px; background:rgba(52, 211, 153, 0.1); border-radius:4px;">✅ Success! Models configured correctly.</span>';
      resultEl.style.color = '#34d399';
    } catch (err) {
      resultEl.innerHTML = `<span style="display:inline-block; padding:4px 8px; background:rgba(248, 113, 113, 0.1); border-radius:4px;">❌ ${err.message}</span>`;
      resultEl.style.color = '#f87171';
    } finally {
      btnEl.textContent = origText;
      btnEl.disabled = false;
    }
  }

  if ($('#testSetupLlmBtn')) {
    $('#testSetupLlmBtn').onclick = () => {
      testLLM(
        $('#setupUrl').value.trim(),
        $('#setupKey').value.trim(),
        $('#setupModelConversation').value,
        $('#setupEmbedding').value,
        $('#testSetupLlmBtn'),
        $('#testSetupLlmResult'),
        $('#setupEmbeddingEngine').value,
        $('#setupLocalEmbeddingModelPath').value
      );
    };
  }

  if ($('#testSettingsLlmBtn')) {
    $('#testSettingsLlmBtn').onclick = () => {
      testLLM(
        $('#ollamaUrl').value.trim(),
        $('#apiKey').value.trim(),
        $('#modelConversation').value,
        $('#embeddingModel').value,
        $('#testSettingsLlmBtn'),
        $('#testSettingsLlmResult'),
        $('#embeddingEngine').value,
        $('#localEmbeddingModelPath').value
      );
    };
  }

  $('#setupSaveBtn').onclick = async () => {
    const mc = $('#setupModelCrawling').value;
    const mconv = $('#setupModelConversation').value;
    const mp = $('#setupModelPlanning').value;
    const me = $('#setupModelExecuting').value;
    const ee = $('#setupEmbeddingEngine').value;
    const em = $('#setupEmbedding').value;
    const lpath = $('#setupLocalEmbeddingModelPath').value;
    if (!mc || !mconv || !mp || !me || (ee === 'openapi' && !em)) {
      toast('Please select models first', true);
      return;
    }
    const c = readConfig();
    c.ollama_url = $('#setupUrl').value.trim();
    c.api_key = $('#setupKey').value.trim();
    c.model_crawling = mc;
    c.model_conversation = mconv;
    c.model_planning = mp;
    c.model_executing = me;
    c.embedding_engine = ee;
    c.embedding_model = em;
    c.local_embedding_model_path = lpath;
    c.context_budget = +$('#setupBudget').value;
    c.output_reserve = Math.floor(c.context_budget * 0.25);
    c.code_budget = Math.floor(c.context_budget * 0.50);
    try {
      await api('/api/config?scope=global', {method:'POST', body:JSON.stringify(c)});
      initialSetupComplete = true;
      $('#globalSetupDialog').close();
      toast('Global setup saved!');
      await refresh();
    } catch(e) {
      toast(`Failed to save: ${e.message}`, true);
    }
  };

  $('#projSetupSaveBtn').onclick = async () => {
    const title = $('#projSetupSessionTitle').value.trim() || 'Initial Setup & Exploration';
    const purpose = $('#projSetupPurpose').value.trim();
    const ignoreList = $('#projSetupIgnore').value.trim();

    try {
      await api('/api/setup/project', {
        method: 'POST',
        body: JSON.stringify({
          initial_session_title: title,
          purpose: purpose,
          ignore_list: ignoreList
        })
      });
      $('#projectSetupDialog').close();
      toast('Project setup complete. Boot scan started.');
      await refresh();
    } catch(e) {
      toast(`Failed project setup: ${e.message}`, true);
    }
  };

  let outPct = 25, codePct = 50;
  
  function updateSliderUI() {
    const t1 = outPct;
    const t2 = outPct + codePct;
    $('#sliceOutput').style.width = t1 + '%';
    $('#sliceCode').style.left = t1 + '%';
    $('#sliceCode').style.width = codePct + '%';
    $('#sliceChat').style.left = t2 + '%';
    $('#sliceChat').style.width = (100 - t2) + '%';
    
    $('#thumb1').style.left = t1 + '%';
    $('#thumb2').style.left = t2 + '%';
    
    const total = +$('#contextBudget').value || 100000;
    const outV = Math.floor(total * (outPct/100));
    const codeV = Math.floor(total * (codePct/100));
    const chatV = total - outV - codeV;
    
    $('#valOutput').textContent = outPct + '% [' + outV.toLocaleString() + ']';
    $('#valCode').textContent = codePct + '% [' + codeV.toLocaleString() + ']';
    $('#valChat').textContent = (100 - t2) + '% [' + chatV.toLocaleString() + ']';
  }

  function setupSlider() {
    let activeThumb = null;
    const slider = $('#budgetSlider');
    if (!slider) return;

    function onMove(e) {
      if (!activeThumb) return;
      const rect = slider.getBoundingClientRect();
      let pct = ((e.clientX - rect.left) / rect.width) * 100;
      pct = Math.max(5, Math.min(95, Math.round(pct)));
      
      if (activeThumb === 1) {
        const t2 = outPct + codePct;
        if (pct >= t2 - 5) pct = t2 - 5;
        outPct = pct;
      } else {
        const t1 = outPct;
        if (pct <= t1 + 5) pct = t1 + 5;
        codePct = pct - t1;
      }
      updateSliderUI();
    }
    
    function onUp() {
      activeThumb = null;
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
    }
    
    $('#thumb1').addEventListener('mousedown', e => {
      activeThumb = 1;
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
    
    $('#thumb2').addEventListener('mousedown', e => {
      activeThumb = 2;
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
    
    $('#contextBudget').addEventListener('input', updateSliderUI);
  }
  setupSlider();

  function updateEmbeddingEngineUI() {
    const ee = $('#embeddingEngine') ? $('#embeddingEngine').value : 'local';
    const see = $('#setupEmbeddingEngine') ? $('#setupEmbeddingEngine').value : 'local';
    
    if ($('#lblLocalEmbeddingModelPath')) {
      $('#lblLocalEmbeddingModelPath').style.display = ee === 'local' ? 'block' : 'none';
      $('#lblEmbeddingModel').style.display = ee === 'local' ? 'none' : 'block';
      $('#localEmbeddingModelPath').required = ee === 'local';
      $('#embeddingModel').required = ee !== 'local';
    }
    
    if ($('#lblSetupLocalEmbeddingModelPath')) {
      $('#lblSetupLocalEmbeddingModelPath').style.display = see === 'local' ? 'block' : 'none';
      $('#lblSetupEmbedding').style.display = see === 'local' ? 'none' : 'block';
      $('#setupLocalEmbeddingModelPath').required = see === 'local';
      $('#setupEmbedding').required = see !== 'local';
    }
  }

  if ($('#embeddingEngine')) $('#embeddingEngine').addEventListener('change', updateEmbeddingEngineUI);
  if ($('#setupEmbeddingEngine')) $('#setupEmbeddingEngine').addEventListener('change', updateEmbeddingEngineUI);

  function fillSettings(c, ignoreText, force = false) {
    if (!c || ($('#settingsDialog').open && !force)) return;
    $('#ollamaUrl').value = c.ollama_url || '';
    $('#apiKey').value = c.api_key || '';
    setSelect('#modelCrawling', c.model_crawling);
    setSelect('#modelConversation', c.model_conversation);
    setSelect('#modelPlanning', c.model_planning);
    setSelect('#modelExecuting', c.model_executing);

    const allSame = (c.model_crawling === c.model_conversation && c.model_conversation === c.model_planning && c.model_planning === c.model_executing);
    $('#syncModels').checked = allSame;
    updateSyncUI();
    
    setSelect('#embeddingEngine', c.embedding_engine || 'local');
    $('#localEmbeddingModelPath').value = c.local_embedding_model_path || '';
    setSelect('#embeddingModel', c.embedding_model);
    if (typeof updateEmbeddingEngineUI === 'function') updateEmbeddingEngineUI();

    $('#contextBudget').value = c.context_budget || 100000;
    const total = c.context_budget || 100000;
    outPct = Math.round(((c.output_reserve || 4000) / total) * 100) || 25;
    codePct = Math.round(((c.code_budget || 8000) / total) * 100) || 50;
    updateSliderUI();
    $('#recentMessages').value = c.recent_messages || 12;
    $('#contextRecentBreadcrumbs').value = c.context_recent_breadcrumbs || 5;
    $('#contextNetworkMarginPercent').value = c.context_network_margin_percent || 2;
    $('#contextPromptMarginPercent').value = c.context_prompt_margin_percent || 15;
    $('#contextCodeMaxFloor').value = c.context_code_budget_max_floor || 8000;
    $('#maxPromptToolOutputBytes').value = c.max_prompt_tool_output_bytes || 30000;
    $('#memoryTopK').value = c.memory_top_k || 8;
    $('#memoryPool').value = c.memory_candidate_pool || 30;
    $('#repairAttempts').value = c.json_repair_attempts || 7;
    setSelect('#evaluationModeSettings', c.evaluation_mode);
    $('#maxPlanDepth').value = c.max_plan_depth || 5;
    $('#maxExecutionLoops').value = c.max_execution_loops || 80;
    $('#maxPrerequisitesPerStep').value = c.max_prerequisites_per_step || 4;
    $('#modelTimeout').value = c.model_timeout_seconds || 600;
    $('#commandTimeout').value = c.command_timeout_seconds || 120;
    $('#llmPollingTimeout').value = c.llm_polling_timeout_seconds || 15;
    $('#serverHeaderTimeout').value = c.server_read_header_timeout_seconds || 10;
    $('#fsWatcherDebounce').value = c.fs_watcher_debounce_ms || 500;
    $('#maxToolOutputBytes').value = c.max_stored_tool_output_bytes || 4194304;
    $('#crawlerSmallFileThreshold').value = c.crawler_small_file_threshold || 200;
    $('#crawlerMaxChunkLines').value = c.crawler_max_chunk_lines || 150;
    $('#memoryLTSDecay').value = c.memory_lts_decay_half_life_hours || 336.0;
    $('#memoryLTSWeight').value = c.memory_lts_max_weight || 0.03;
    $('#memorySessionDecay').value = c.memory_session_decay_half_life_hours || 2.0;
    $('#memorySessionWeight').value = c.memory_session_max_weight || 0.20;
    $('#memoryRerankGates').value = c.memory_rerank_gates || 3;
    $('#memoryPoolExpansion').value = c.memory_pool_expansion_limit || 4;
    $('#contextRecentBreadcrumbs').value = c.context_recent_breadcrumbs || 5;
    $('#blindActionThreshold').value = c.blind_action_threshold || 5;
    $('#contextNetworkMarginPercent').value = c.context_network_margin_percent || 2;
    $('#contextPromptMarginPercent').value = c.context_prompt_margin_percent || 15;
    $('#contextCodeMaxFloor').value = c.context_code_budget_max_floor || 8000;
    $('#projectInstructions').value = c.project_instructions || '';
    $('#settingsIgnore').value = ignoreText || '';
    renderPerms(c.permissions || {});
  }

  function setSelect(sel, val) {
    const el = $(sel);
    if (!el) return;
    if (val && !([...el.options].some(o => o.value === val))) {
      const o = document.createElement('option');
      o.value = val;
      o.textContent = val;
      el.appendChild(o);
    }
    el.value = val || '';
  }

  function renderPerms(p) {
    const mode = p.mode || 'ask_selected';
    const radio = $(`input[name="permMode"][value="${mode}"]`);
    if (radio) radio.checked = true;
    const ask = p.ask_for || {};
    $('#permChecks').innerHTML = permissionTools.map(k => `<label><input type="checkbox" data-perm="${esc(k)}" ${ask[k] ? 'checked' : ''}> ${esc(k)}</label>`).join('');
  }

  function readConfig() {
    const ask = {};
    $$('[data-perm]').forEach(x => ask[x.dataset.perm] = x.checked);
    const total = +$('#contextBudget').value || 100000;
    return {
      ollama_url: $('#ollamaUrl').value.trim(),
      api_key: $('#apiKey').value.trim(),
      model_crawling: $('#modelCrawling').value,
      model_conversation: $('#modelConversation').value,
      model_planning: $('#modelPlanning').value,
      model_executing: $('#modelExecuting').value,
      embedding_engine: $('#embeddingEngine').value,
      embedding_model: $('#embeddingModel').value,
      local_embedding_model_path: $('#localEmbeddingModelPath').value,
      context_budget: total,
      output_reserve: Math.floor(total * (outPct/100)),
      code_budget: Math.floor(total * (codePct/100)),
      recent_messages: +$('#recentMessages').value,
      memory_top_k: +$('#memoryTopK').value,
      context_recent_breadcrumbs: +$('#contextRecentBreadcrumbs').value,
      context_network_margin_percent: +$('#contextNetworkMarginPercent').value,
      context_prompt_margin_percent: +$('#contextPromptMarginPercent').value,
      context_code_budget_max_floor: +$('#contextCodeMaxFloor').value,
      max_prompt_tool_output_bytes: +$('#maxPromptToolOutputBytes').value,
      memory_candidate_pool: +$('#memoryPool').value,
      json_repair_attempts: +$('#repairAttempts').value,
      evaluation_mode: $('#evaluationModeSettings').value,
      max_plan_depth: +$('#maxPlanDepth').value,
      max_execution_loops: +$('#maxExecutionLoops').value,
      max_prerequisites_per_step: +$('#maxPrerequisitesPerStep').value,
      model_timeout_seconds: +$('#modelTimeout').value,
      command_timeout_seconds: +$('#commandTimeout').value,
      llm_polling_timeout_seconds: +$('#llmPollingTimeout').value,
      server_read_header_timeout_seconds: +$('#serverHeaderTimeout').value,
      fs_watcher_debounce_ms: +$('#fsWatcherDebounce').value,
      max_stored_tool_output_bytes: +$('#maxToolOutputBytes').value,
      crawler_small_file_threshold: +$('#crawlerSmallFileThreshold').value,
      crawler_max_chunk_lines: +$('#crawlerMaxChunkLines').value,
      memory_lts_decay_half_life_hours: +$('#memoryLTSDecay').value,
      memory_lts_max_weight: +$('#memoryLTSWeight').value,
      memory_session_decay_half_life_hours: +$('#memorySessionDecay').value,
      memory_session_max_weight: +$('#memorySessionWeight').value,
      memory_rerank_gates: +$('#memoryRerankGates').value,
      memory_pool_expansion_limit: +$('#memoryPoolExpansion').value,
      context_recent_breadcrumbs: +$('#contextRecentBreadcrumbs').value,
      blind_action_threshold: +$('#blindActionThreshold').value,
      context_network_margin_percent: +$('#contextNetworkMarginPercent').value,
      context_prompt_margin_percent: +$('#contextPromptMarginPercent').value,
      context_code_budget_max_floor: +$('#contextCodeMaxFloor').value,
      project_instructions: $('#projectInstructions').value,
      permissions: {mode: $('input[name="permMode"]:checked')?.value || 'ask_selected', ask_for: ask}
    };
  }

  async function saveCfg(scope) {
    try {
      const cfg = readConfig();
      await api('/api/config?scope=' + scope, {method:'POST', body:JSON.stringify(cfg)});
      
      if (scope === 'project') {
        const ignoreList = $('#settingsIgnore').value;
        await api('/api/loaignore', {method:'POST', body:JSON.stringify({ignore_list: ignoreList})});
      }

      const wasIncomplete = !$('#settingsDialog').open || $('#closeSettings').style.display === 'none';
      toast(`Settings saved (${scope})`);
      
      if (wasIncomplete && cfg.model_crawling && (cfg.embedding_engine === 'local' || cfg.embedding_model)) {
        initialSetupComplete = true;
        api('/api/reindex', {method:'POST'});
        toast("Initializing memory index...");
      }
      await refresh();
    } catch (e) {
      toast(e.message, true);
    }
  }

  $('#saveProject').onclick = () => saveCfg('project');
  $('#saveGlobal').onclick = () => saveCfg('global');
  
  $('#resetDefaults').onclick = async () => {
    try {
      const def = await api('/api/config/default');
      // Only merge current ignoreText, do not reset it since it's not part of config struct
      fillSettings(def, $('#settingsIgnore').value, true);
      toast("Settings reset to defaults. Click Save to apply.");
    } catch (e) {
      toast("Failed to load defaults: " + e.message, true);
    }
  };

  renderLogFilters();
  function updateSyncUI() {
    const sync = $('#syncModels').checked;
    $('#modelConversation').parentElement.style.display = sync ? 'none' : '';
    $('#modelPlanning').parentElement.style.display = sync ? 'none' : '';
    $('#modelExecuting').parentElement.style.display = sync ? 'none' : '';
    const txt = $('#textModelCrawling');
    if (txt) txt.textContent = sync ? 'Inference model' : 'Crawling model';
  }

  function updateSetupSyncUI() {
    const sync = $('#setupSyncModels').checked;
    $('#setupModelConversation').parentElement.style.display = sync ? 'none' : '';
    $('#setupModelPlanning').parentElement.style.display = sync ? 'none' : '';
    $('#setupModelExecuting').parentElement.style.display = sync ? 'none' : '';
    const txt = $('#textSetupModelCrawling');
    if (txt) txt.textContent = sync ? 'Inference Model' : 'Crawling Model';
  }

  $('#syncModels')?.addEventListener('change', (e) => {
    updateSyncUI();
    if (e.target.checked) {
      const val = $('#modelCrawling').value;
      $('#modelConversation').value = val;
      $('#modelPlanning').value = val;
      $('#modelExecuting').value = val;
    }
  });

  $('#modelCrawling')?.addEventListener('change', (e) => {
    if ($('#syncModels').checked) {
      const val = e.target.value;
      $('#modelConversation').value = val;
      $('#modelPlanning').value = val;
      $('#modelExecuting').value = val;
    }
  });

  $('#setupSyncModels')?.addEventListener('change', (e) => {
    updateSetupSyncUI();
    if (e.target.checked) {
      const val = $('#setupModelCrawling').value;
      $('#setupModelConversation').value = val;
      $('#setupModelPlanning').value = val;
      $('#setupModelExecuting').value = val;
    }
  });

  $('#setupModelCrawling')?.addEventListener('change', (e) => {
    if ($('#setupSyncModels').checked) {
      const val = e.target.value;
      $('#setupModelConversation').value = val;
      $('#setupModelPlanning').value = val;
      $('#setupModelExecuting').value = val;
    }
  });

  $('#copyContextBtn')?.addEventListener('click', () => {
    if (renderSignatures.context) {
      navigator.clipboard.writeText(renderSignatures.context).then(() => toast('Context copied to clipboard!'));
    }
  });

  const modeUpdateHandler = async () => {
    try {
      await api('/api/session/modes', {
        method: 'POST',
        body: JSON.stringify({
          complexity_mode: document.querySelector('input[name="complexityModeCtrl"]:checked')?.value,
          read_only: $('#readOnlyCtrl').checked
        })
      });
      refresh();
    } catch (err) {
      toast('Failed to update session mode: ' + err.message, true);
    }
  };
  $$('input[name="complexityModeCtrl"]').forEach(el => el.addEventListener('change', modeUpdateHandler));

  $('#readOnlyCtrl')?.addEventListener('change', modeUpdateHandler);

  $('#memorySearchInput')?.addEventListener('input', () => {
    applyMemoryFilter();
  });

  $('#closeMemoryEdit')?.addEventListener('click', () => {
    $('#memoryEditDialog').close();
  });

  $('#memoryEditSaveBtn')?.addEventListener('click', async () => {
    const id = parseInt($('#memoryEditDialog').dataset.id);
    const text = $('#memoryEditTextInput').value;
    if (!text.trim()) {
      toast('Memory text cannot be empty', true);
      return;
    }
    const btn = $('#memoryEditSaveBtn');
    btn.disabled = true;
    btn.textContent = 'SAVING...';
    try {
      await api('/api/memory/edit', {
        method: 'POST',
        body: JSON.stringify({ id, text })
      });
      toast('Memory updated successfully!');
      $('#memoryEditDialog').close();
      refresh();
    } catch (err) {
      toast('Error updating memory: ' + err.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = 'SAVE CHANGES';
    }
  });

  // Graph interactive controls
  $('#graphZoomInBtn')?.addEventListener('click', () => {
      if (network) {
          const scale = network.getScale();
          network.moveTo({ scale: scale * 1.2, animation: { duration: 300, easingFunction: 'easeInOutQuad' } });
      }
  });
  $('#graphZoomOutBtn')?.addEventListener('click', () => {
      if (network) {
          const scale = network.getScale();
          network.moveTo({ scale: scale / 1.2, animation: { duration: 300, easingFunction: 'easeInOutQuad' } });
      }
  });
  $('#graphFitBtn')?.addEventListener('click', () => {
      if (network) {
          network.fit({ animation: { duration: 300, easingFunction: 'easeInOutQuad' } });
      }
  });
  $('#closeStepDetails')?.addEventListener('click', () => {
      $('#stepDetailsDialog').close();
  });
  $('#closeAboutBtn')?.addEventListener('click', () => {
      $('#aboutDialog').close();
  });

  // Pane collapse controls
  const workspaceContainer = $('.workspace');
  $('#collapseChatBtn')?.addEventListener('click', () => {
    if (workspaceContainer.classList.contains('chat-collapsed')) {
      workspaceContainer.classList.remove('chat-collapsed');
    } else {
      workspaceContainer.classList.add('chat-collapsed');
      workspaceContainer.classList.remove('inspect-collapsed');
    }
  });

  $('#collapseInspectBtn')?.addEventListener('click', () => {
    if (workspaceContainer.classList.contains('inspect-collapsed')) {
      workspaceContainer.classList.remove('inspect-collapsed');
    } else {
      workspaceContainer.classList.add('inspect-collapsed');
      workspaceContainer.classList.remove('chat-collapsed');
    }
  });

  // Ensure initial states are set
  updateSyncUI();
  updateSetupSyncUI();

  // Attachments and Uploads logic
  function renderUploads(uploads) {
    const listEl = $('#uploadListContainer');
    if (!listEl) return;
    if (!uploads || uploads.length === 0) {
      listEl.innerHTML = '<div class="empty">No files uploaded yet</div>';
      return;
    }

    listEl.innerHTML = '';
    uploads.forEach(up => {
      const row = document.createElement('div');
      row.style.cssText = 'display:flex; justify-content:space-between; align-items:center; padding:8px; border-bottom:1px solid var(--border);';
      
      const left = document.createElement('div');
      left.style.cssText = 'display:flex; flex-direction:column;';
      left.innerHTML = `<span style="font-weight:bold; font-size:12px;">${up.filename}</span>
                        <span style="font-size:10px; color:var(--muted);">${new Date(up.created_at).toLocaleString()}</span>`;
      
      const right = document.createElement('div');
      right.style.cssText = 'display:flex; gap: 8px; align-items:center;';
      
      const roLabel = document.createElement('label');
      roLabel.style.cssText = 'font-size:10px; display:flex; align-items:center; gap:4px; cursor:pointer; color:var(--text);';
      const roCheck = document.createElement('input');
      roCheck.type = 'checkbox';
      roCheck.checked = up.read_only;
      roCheck.onchange = () => toggleUploadReadOnly(up.id, roCheck.checked);
      roLabel.appendChild(roCheck);
      roLabel.append(' READ_ONLY');
      
      const dlBtn = document.createElement('a');
      dlBtn.className = 'icon-btn';
      dlBtn.style.cssText = 'color:var(--cyan); border:1px solid var(--line); background:var(--bg); cursor:pointer; font-size:12px; padding:2px 6px; text-decoration: none;';
      dlBtn.innerText = '↓';
      dlBtn.href = '/api/upload/' + up.id + '/download';
      dlBtn.target = '_blank';
      
      const delBtn = document.createElement('button');
      delBtn.className = 'icon-btn';
      delBtn.style.cssText = 'color:var(--bad); border:1px solid var(--line); background:var(--bg); cursor:pointer; font-size:12px; padding:2px 6px;';
      delBtn.innerText = '🗑';
      delBtn.onclick = () => deleteUpload(up.id);
      
      right.appendChild(roLabel);
      right.appendChild(dlBtn);
      right.appendChild(delBtn);
      
      row.appendChild(left);
      row.appendChild(right);
      listEl.appendChild(row);
    });
  }

  function renderAttachments(attachments) {
    const countEl = $('#attachmentsCount');
    const listEl = $('#activeAttachmentsList');
    if (!countEl || !listEl) return;
    
    countEl.innerText = `📎 ${attachments.length}`;
    
    if (attachments.length === 0) {
      listEl.innerHTML = '<div class="muted" style="font-size:11px; text-align:center;">No active attachments</div>';
      return;
    }
    
    listEl.innerHTML = '';
    attachments.forEach(att => {
      const row = document.createElement('div');
      row.style.cssText = 'display:flex; justify-content:space-between; align-items:center; padding:4px 8px; border:1px solid var(--border); margin-bottom:4px; border-radius:4px; background:var(--bg);';
      
      const nameParts = att.virtual_path.split('/');
      const name = nameParts[nameParts.length - 1];
      
      const nameSpan = document.createElement('span');
      nameSpan.style.cssText = 'font-size:11px; font-family:var(--mono); text-overflow:ellipsis; overflow:hidden; white-space:nowrap;';
      nameSpan.innerText = name;
      
      const delBtn = document.createElement('button');
      delBtn.innerText = '×';
      delBtn.title = 'Detach';
      delBtn.style.cssText = 'background:transparent; border:none; color:var(--bad); cursor:pointer; padding:0 4px; font-weight:bold; font-size:14px;';
      delBtn.onclick = () => detachFile(att.id);
      
      row.appendChild(nameSpan);
      row.appendChild(delBtn);
      listEl.appendChild(row);
    });
  }

  async function handleFileUpload(files, autoAttach = false) {
    if (!files || files.length === 0) return;
    
    const formData = new FormData();
    for (let i = 0; i < files.length; i++) {
      formData.append('files', files[i]);
    }
    
    try {
      const res = await fetch('/api/upload', {
        method: 'POST',
        headers: { 'X-Loa-Token': token },
        body: formData
      });
      if (!res.ok) throw new Error(await res.text());
      const data = await res.json();
      toast(`Uploaded ${data.uploads.length} file(s)`);
      
      if (autoAttach) {
        const ids = data.uploads.map(u => u.id);
        await attachFiles(ids, 'upload');
      }
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function toggleUploadReadOnly(id, readOnly) {
    try {
      const res = await fetch('/api/upload/readonly', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Loa-Token': token },
        body: JSON.stringify({ id, read_only: readOnly })
      });
      if (!res.ok) throw new Error(await res.text());
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function deleteUpload(id) {
    if (!confirm('Are you sure you want to delete this upload?')) return;
    try {
      const res = await fetch('/api/upload/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Loa-Token': token },
        body: JSON.stringify({ id })
      });
      if (!res.ok) throw new Error(await res.text());
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function attachFiles(ids, type) {
    try {
      const res = await fetch('/api/attach', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Loa-Token': token },
        body: JSON.stringify({ ids, type })
      });
      if (!res.ok) throw new Error(await res.text());
    } catch (e) {
      toast(e.message, true);
    }
  }

  async function detachFile(id) {
    try {
      const res = await fetch('/api/attach/delete', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json', 'X-Loa-Token': token },
        body: JSON.stringify({ id })
      });
      if (!res.ok) throw new Error(await res.text());
    } catch (e) {
      toast(e.message, true);
    }
  }

  $('#uploadBtn')?.addEventListener('click', () => $('#uploadInput').click());
  $('#uploadInput')?.addEventListener('change', (e) => {
    handleFileUpload(e.target.files, false);
    e.target.value = '';
  });

  $('#chatUploadBtn')?.addEventListener('click', () => $('#chatUploadInput').click());
  $('#chatUploadInput')?.addEventListener('change', (e) => {
    handleFileUpload(e.target.files, true);
    e.target.value = '';
  });

  $('.attachments-header')?.addEventListener('click', (e) => {
    const p = $('#attachmentsOverlay');
    if (p.classList.contains('collapsed')) {
      p.classList.remove('collapsed');
      $('#attachmentsContent').style.display = 'block';
      $('#attachmentsToggleIcon').innerText = '▲';
    } else {
      p.classList.add('collapsed');
      $('#attachmentsContent').style.display = 'none';
      $('#attachmentsToggleIcon').innerText = '▼';
    }
  });

  $('#openAttachModalBtn')?.addEventListener('click', () => {
    if (!$('#attachModal').open) {
      populateAttachModal();
      $('#attachModal').showModal();
    }
  });
  $('#closeAttachModal')?.addEventListener('click', () => $('#attachModal').close());

  function populateAttachModal() {
    const uploadsContainer = $('#attachModalUploads');
    const artifactsContainer = $('#attachModalArtifacts');
    uploadsContainer.innerHTML = '';
    artifactsContainer.innerHTML = '';
    
    if (snapshot && snapshot.state && snapshot.state.uploads) {
      snapshot.state.uploads.forEach(up => {
        const label = document.createElement('label');
        label.style.cssText = 'display:flex; align-items:center; gap:8px; font-size:12px; cursor:pointer; padding:4px; border:1px solid transparent;';
        label.onmouseenter = () => label.style.borderColor = 'var(--border)';
        label.onmouseleave = () => label.style.borderColor = 'transparent';
        
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.value = up.id;
        cb.dataset.type = 'upload';
        
        const name = document.createElement('span');
        name.innerText = up.filename;
        name.style.flex = 1;
        
        label.appendChild(cb);
        label.appendChild(name);
        uploadsContainer.appendChild(label);
      });
    }

    if (snapshot && snapshot.artifacts) {
      const taskIds = Object.keys(snapshot.artifacts).map(Number).sort((a, b) => b - a);
      taskIds.forEach(tid => {
        const h = document.createElement('div');
        h.innerText = 'Task ' + tid;
        h.style.cssText = 'font-size:10px; color:var(--cyan); margin-top:10px; font-weight:bold;';
        artifactsContainer.appendChild(h);

        snapshot.artifacts[tid].forEach(file => {
          const path = `.loa/artifacts/${activeSessionID}/task-${tid}/${file}`;
          const label = document.createElement('label');
          label.style.cssText = 'display:flex; align-items:center; gap:8px; font-size:12px; cursor:pointer; padding:4px; border:1px solid transparent;';
          label.onmouseenter = () => label.style.borderColor = 'var(--border)';
          label.onmouseleave = () => label.style.borderColor = 'transparent';
          
          const cb = document.createElement('input');
          cb.type = 'checkbox';
          cb.value = path;
          cb.dataset.type = 'artifact';
          
          const name = document.createElement('span');
          name.innerText = file;
          
          label.appendChild(cb);
          label.appendChild(name);
          artifactsContainer.appendChild(label);
        });
      });
    }
  }

  $('#addSelectedAttachmentsBtn')?.addEventListener('click', async () => {
    const checked = $$('#attachModal input[type="checkbox"]:checked');
    const uploadIds = [];
    const artifactIds = [];
    checked.forEach(cb => {
      if (cb.dataset.type === 'upload') uploadIds.push(cb.value);
      if (cb.dataset.type === 'artifact') artifactIds.push(cb.value);
    });
    
    if (uploadIds.length > 0) {
      await attachFiles(uploadIds, 'upload');
    }
    if (artifactIds.length > 0) {
      await attachFiles(artifactIds, 'artifact');
    }
    
    $('#attachModal').close();
  });

  bootstrap().catch(e => toast(e.message, true));
})();
