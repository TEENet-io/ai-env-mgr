(() => {
  const search = document.getElementById('tp-search');
  if (search) search.addEventListener('input', () => {
    const query = search.value.trim().toLocaleLowerCase();
    document.querySelectorAll('[data-tp-ticket-row]').forEach(row => {
      row.hidden = !row.textContent.toLocaleLowerCase().includes(query);
    });
  });

  const form = document.getElementById('tp-request-form');
  if (!form) return;
  const categoryButtons = [...document.querySelectorAll('[data-tp-category]')];
  const kindButtons = [...document.querySelectorAll('[data-tp-kind]')];
  const fieldsets = [...form.querySelectorAll('[data-tp-fields]')];
  const typePanel = document.getElementById('tp-type-panel');
  const formPanel = document.getElementById('tp-form-panel');
  const review = document.getElementById('tp-review');
  const reviewContent = document.getElementById('tp-review-content');
  const kindInput = document.getElementById('tp-request-kind');
  const submit = document.getElementById('tp-submit');
  const titles = Object.fromEntries(kindButtons.map(button => [button.dataset.tpKind, button.firstChild.textContent.trim()]));
  const problemKinds = new Set(['desktop-problem', 'ai-problem', 'tool-problem', 'other-problem']);
  const optional = document.getElementById('tp-optional');
  const noteInput = form.querySelector('[name=note]');
  const desiredInput = form.querySelector('[name=desired_at]');
  const maxFiles = 5, maxFileSize = 5 << 20;
  let selectedCategory = '';
  const formError = document.getElementById('tp-form-error');
  const reviewConfirm = document.getElementById('tp-review-confirm');
  let confirmed = false;
  let sending = false;

  function selectCategory(category) {
    selectedCategory = category;
    categoryButtons.forEach(button => button.classList.toggle('active', button.dataset.tpCategory === category));
    kindButtons.forEach(button => { button.hidden = button.dataset.tpParent !== category; });
    typePanel.hidden = false;
    formPanel.hidden = true;
    review.hidden = true;
    if (category === 'tool' || category === 'other') {
      const button = kindButtons.find(item => item.dataset.tpParent === category);
      if (button) selectKind(button.dataset.tpKind);
    } else {
      typePanel.scrollIntoView({behavior:'smooth', block:'nearest'});
    }
  }

  // A problem report has no handling time. It normally has no note either,
  // unless an earlier version already carries one. The block starts folded
  // and opens only for what is already filled in.
  function syncOptional(kind) {
    const problem = problemKinds.has(kind);
    const hasNote = Boolean(noteInput.value.trim());
    optional.hidden = problem && !hasNote;
    noteInput.disabled = optional.hidden;
    desiredInput.disabled = problem;
    document.getElementById('tp-desired-field').hidden = problem;
    document.getElementById('tp-optional-title').textContent = problem ? '補充說明' : '補充說明和希望處理時間';
    optional.open = hasNote || (!problem && Boolean(desiredInput.value.trim()));
  }

  function selectKind(kind) {
    kindInput.value = kind;
    formError.hidden = true;
    fieldsets.forEach(fieldset => {
      const active = fieldset.dataset.tpFields === kind;
      fieldset.disabled = !active;
      fieldset.hidden = !active;
    });
    syncOptional(kind);
    document.getElementById('tp-form-title').textContent = titles[kind] || '填寫信息';
    submit.textContent = problemKinds.has(kind) ? '提交工單' : '核對申請';
    typePanel.hidden = true;
    formPanel.hidden = false;
    review.hidden = true;
    formPanel.scrollIntoView({behavior:'smooth', block:'start'});
  }

  categoryButtons.forEach(button => button.addEventListener('click', () => selectCategory(button.dataset.tpCategory)));
  kindButtons.forEach(button => button.addEventListener('click', () => selectKind(button.dataset.tpKind)));
  document.getElementById('tp-back-categories').addEventListener('click', () => {
    typePanel.hidden = true;
    formPanel.hidden = true;
    review.hidden = true;
    categoryButtons.forEach(button => button.classList.remove('active'));
  });
  document.getElementById('tp-back-types').addEventListener('click', () => {
    formPanel.hidden = true;
    typePanel.hidden = false;
    typePanel.scrollIntoView({behavior:'smooth', block:'start'});
  });

  const rows = [...form.querySelectorAll('.tp-employee-row')];
  document.getElementById('tp-add-employee').addEventListener('click', () => {
    const row = rows.find(item => item.hidden);
    if (row) { row.hidden = false; row.querySelector('input')?.focus(); }
    if (!rows.some(item => item.hidden)) document.getElementById('tp-add-employee').hidden = true;
  });

  form.querySelectorAll('[data-tp-select-all]').forEach(toggle => {
    const targets = [...toggle.closest('.tp-targets').querySelectorAll('input[name=employee_ids]')];
    const email = form.dataset.customerEmail?.toLocaleLowerCase();
    if (email) targets.forEach(target => { target.checked = target.dataset.employeeEmail?.toLocaleLowerCase() === email; });
    toggle.checked = targets.length > 0 && targets.every(item => item.checked);
    toggle.indeterminate = targets.some(item => item.checked) && !toggle.checked;
    toggle.addEventListener('change', () => targets.forEach(target => { target.checked = toggle.checked; }));
    targets.forEach(target => target.addEventListener('change', () => {
      toggle.checked = targets.every(item => item.checked);
      toggle.indeterminate = targets.some(item => item.checked) && !toggle.checked;
    }));
  });

  const websiteAction = document.getElementById('tp-website-action');
  const expiresField = document.getElementById('tp-expires-field');
  function syncExpires() {
    const temporary = websiteAction.value === 'temporary';
    const input = expiresField.querySelector('input');
    expiresField.hidden = !temporary;
    input.disabled = !temporary;
    input.required = temporary;
  }
  websiteAction.addEventListener('change', syncExpires);
  syncExpires();

  const editEmployee = document.getElementById('tp-edit-employee');
  editEmployee.addEventListener('change', () => {
    const person = editEmployee.selectedOptions[0]?.dataset || {};
    for (const field of ['name', 'email', 'number']) {
      document.getElementById(`tp-current-${field}`).textContent = person[field] || '—';
      editEmployee.closest('fieldset').querySelector(`[name="new_${field}"]`).value = '';
    }
  });

  function validateBusinessFields() {
    const active = fieldsets.find(item => !item.disabled);
    if (!active) return false;
    const kind = kindInput.value;
    const named = name => active.querySelector(`[name="${name}"]`);
    let invalid = null;
    if (kind === 'account-add') {
      const filled = rows.filter(row => [...row.querySelectorAll('input')].some(input => input.value.trim()));
      if (!filled.length) invalid = rows[0].querySelector('input');
      for (const row of filled) {
        const [name, email] = row.querySelectorAll('input');
        if (!name.value.trim()) { invalid = name; break; }
        if (!email.value.trim() || !email.checkValidity()) { invalid = email; break; }
      }
    }
    if (kind.startsWith('account-') && kind !== 'account-add' && !named('target_employee').value) invalid = named('target_employee');
    if (kind === 'account-edit' && !['new_name','new_email','new_number'].some(name => named(name).value.trim())) invalid = named('new_name');
    if (kind.startsWith('software-')) {
      if (![...active.querySelectorAll('input[name=employee_ids]')].some(input => input.checked)) invalid = active.querySelector('input[name=employee_ids]');
      if (!named('software').value.trim()) invalid = named('software');
    }
    if (kind === 'quota' && !active.querySelector('input[name=employee_ids]:checked')) invalid = active.querySelector('input[name=employee_ids]');
    if (kind === 'website' && !active.querySelector('input[name=sites]:checked') && !named('other_site').value.trim()) invalid = named('other_site');
    if (kind === 'tool-problem' && !named('tool').value.trim()) invalid = named('tool');
    if (problemKinds.has(kind) && !named('problem').value.trim()) invalid = named('problem');
    const upload = named('files');
    if (upload && upload.files.length) {
      const files = [...upload.files];
      const kept = Number(form.dataset.keptFiles || 0);
      if (files.length + kept > maxFiles) { upload.setCustomValidity(`最多 ${maxFiles} 個檔案${kept ? `，已有 ${kept} 個` : ''}`); invalid = upload; }
      else if (files.some(file => file.size > maxFileSize)) { upload.setCustomValidity('單個檔案不能超過 5 MB'); invalid = upload; }
      else {
        // The browser's guess catches the wrong kind of file early. The server decides from the content.
        const wrong = files.find(file => file.type && !upload.accept.split(',').includes(file.type));
        if (wrong) { upload.setCustomValidity(`${wrong.name} 不是支持的檔案類型`); invalid = upload; }
      }
      if (invalid === upload) { upload.reportValidity(); upload.addEventListener('change', () => upload.setCustomValidity(''), {once:true}); return false; }
    }
    if (invalid) {
      invalid.focus();
      invalid.setCustomValidity('請完成此項');
      invalid.reportValidity();
      invalid.addEventListener('input', () => invalid.setCustomValidity(''), {once:true});
      invalid.addEventListener('change', () => invalid.setCustomValidity(''), {once:true});
      return false;
    }
    return form.reportValidity();
  }

  function showReview() {
    reviewContent.replaceChildren();
    const list = document.createElement('dl');
    const add = (label, value) => {
      const dt = document.createElement('dt'); const dd = document.createElement('dd');
      dt.textContent = label; dd.textContent = value; list.append(dt, dd);
    };
    add('事項', titles[kindInput.value]);
    const active = fieldsets.find(item => !item.disabled);
    if (kindInput.value === 'account-add') {
      add('新增員工', rows.filter(row => [...row.querySelectorAll('input')].some(input => input.value.trim())).map(row => {
        const inputs = row.querySelectorAll('input');
        return `${inputs[0].value.trim()} · ${inputs[1].value.trim()}${inputs[2].value.trim() ? ' · '+inputs[2].value.trim() : ''}`;
      }).join('\n'));
    } else {
      const select = active.querySelector('select[name=target_employee]');
      if (select) add('員工', select.selectedOptions[0]?.textContent || '');
    }
    const targets = [...active.querySelectorAll('input[name=employee_ids]:checked')];
    if (targets.length) add('適用員工', targets.map(input => input.dataset.label).join('\n'));
    active.querySelectorAll('input:not([type=checkbox]),textarea,select').forEach(input => {
      if (input.name.startsWith('employee_') || input.name.startsWith('work_email_') || input.name === 'target_employee' || input.name === 'expires' && input.closest('label').hidden || !input.value.trim()) return;
      const label = input.closest('label')?.firstChild?.textContent.trim() || input.name;
      add(label, input.tagName === 'SELECT' ? input.selectedOptions[0]?.textContent || '' : input.value.trim());
    });
    const sites = [...active.querySelectorAll('input[name=sites]:checked')].map(input => input.value);
    if (sites.length) add('網站', sites.join('、'));
    const note = form.querySelector('[name=note]').value.trim();
    if (note) add('補充說明', note);
    const desired = form.querySelector('[name=desired_at]').value;
    if (desired) add('希望處理時間', desired);
    reviewContent.append(list);
    formPanel.hidden = true;
    review.hidden = false;
    review.scrollIntoView({behavior:'smooth', block:'start'});
  }

  function setSending(value) {
    sending = value;
    submit.disabled = value;
    reviewConfirm.disabled = value;
  }

  // A refused request comes back to the form with everything still filled in.
  // The request key makes trying again safe.
  function showFormError(message) {
    confirmed = false;
    setSending(false);
    formError.textContent = message;
    formError.hidden = false;
    review.hidden = true;
    formPanel.hidden = false;
    formError.scrollIntoView({behavior:'smooth', block:'center'});
  }

  async function send() {
    if (sending) return;
    setSending(true);
    formError.hidden = true;
    try {
      const response = await fetch(form.getAttribute('action'), {method:'POST', body:new FormData(form)});
      if (response.ok || response.redirected) { location.href = response.url; return; }
      const plain = (response.headers.get('Content-Type') || '').startsWith('text/plain');
      showFormError((plain && (await response.text()).trim()) || '提交失敗，請稍後重試');
    } catch {
      showFormError('網絡連接失敗，已填內容還在，請重試');
    }
  }

  form.addEventListener('submit', event => {
    if (!kindInput.value) return;
    event.preventDefault();
    if (confirmed) { send(); return; }
    if (!validateBusinessFields()) return;
    if (problemKinds.has(kindInput.value)) send(); else showReview();
  });
  document.getElementById('tp-review-back').addEventListener('click', () => { review.hidden = true; formPanel.hidden = false; });
  reviewConfirm.addEventListener('click', () => { confirmed = true; form.requestSubmit(); });

  const edit = form.dataset.editJson ? JSON.parse(form.dataset.editJson) : null;
  if (edit) {
    const details = edit.details;
    selectCategory(details.category);
    selectKind(edit.kind);
    document.querySelector('.tp-new').hidden = true;
    document.getElementById('tp-back-types').hidden = true;
    const active = fieldsets.find(item => !item.disabled);
    const put = (name, value) => {
      const input = active.querySelector(`[name="${name}"]`);
      if (input && value != null) input.value = String(value);
    };
    if (edit.kind === 'account-add') {
      (details.employees || []).forEach((employee, index) => {
        const row = rows[index];
        if (!row) return;
        row.hidden = false;
        const inputs = row.querySelectorAll('input');
        inputs[0].value = employee.name || '';
        inputs[1].value = employee.email || '';
        inputs[2].value = employee.number || '';
      });
    }
    put('target_employee', details.target_employee?.id);
    if (edit.kind === 'account-edit') editEmployee.dispatchEvent(new Event('change'));
    if (edit.kind === 'account-toggle') put('account_action', details.action);
    if (edit.kind === 'account-edit') {
      put('new_name', details.changes?.name);
      put('new_email', details.changes?.email);
      put('new_number', details.changes?.number);
    }
    if (edit.kind.startsWith('software-')) {
      const ids = new Set((details.target_employees || []).map(employee => employee.id));
      active.querySelectorAll('input[name=employee_ids]').forEach(input => { input.checked = ids.has(input.value); });
      active.querySelector('input[name=employee_ids]')?.dispatchEvent(new Event('change'));
      put('software', details.software);
      put('min_version', details.min_version);
      if (edit.kind === 'software-change') put('software_action', details.action);
    }
    if (edit.kind === 'website') {
      put('website_action', details.action);
      active.querySelectorAll('input[name=sites]').forEach(input => { input.checked = (details.sites || []).includes(input.value); });
      put('other_site', details.other_site);
      put('expires', (details.expires || '').replace('T', ' '));
      websiteAction.dispatchEvent(new Event('change'));
    }
    put('problem', details.problem);
    put('occurred_at', details.occurred_at);
    if (details.tool) {
      // A tool that has since left the list is still what this version names.
      const tool = document.getElementById('tp-tool');
      if (![...tool.options].some(option => option.value === details.tool)) tool.add(new Option(details.tool, details.tool), 1);
      tool.value = details.tool;
    }
    noteInput.value = details.note || '';
    desiredInput.value = (details.desired_at || '').replace('T', ' ');
    syncOptional(edit.kind);
    const kept = details.attachments || [];
    if (kept.length) {
      form.dataset.keptFiles = kept.length;
      const hint = document.createElement('p');
      hint.className = 'tp-hint';
      hint.textContent = `已上傳的檔案會保留：${kept.map(file => file.name).join('、')}`;
      active.querySelector('[data-tp-upload]')?.closest('label').after(hint);
    }
  } else {
    const initial = new URLSearchParams(location.search).get('category');
    if (initial && categoryButtons.some(button => button.dataset.tpCategory === initial)) selectCategory(initial);
  }
})();
