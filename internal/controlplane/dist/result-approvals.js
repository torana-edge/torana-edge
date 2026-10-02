/* Hallmark · pre-emit critique: P5 H4 E4 S5 R5 V4
 * Existing Torana tokens and shell; focused approval workbench. */
globalThis.ToranaResultApprovals = (() => {
  let generation = 0;
  let cursor = '';
  let items = [];
  const node = (tag, text, className) => {
    const el = document.createElement(tag);
    el.textContent = text;
    if (className) el.className = className;
    return el;
  };
  const referenceOK = ref => typeof ref === 'string' && /^tr_[0-9a-f]{64}$/.test(ref);
  function render() {
    const container = document.getElementById('resultApprovals');
    container.replaceChildren();
    if (!items.length) {
      container.append(node('p', 'No withheld results yet. When PII withholds a result, your agent can request review through Torana MCP. Reload this page to see it.', 'empty-state'));
      return;
    }
    const sorted = [...items].sort((a, b) => (a.status === 'pending' ? 0 : 1) - (b.status === 'pending' ? 0 : 1));
    for (const item of sorted) {
      const row = node('article', '', 'result-approval');
      row.append(node('h4', `${item.plugin} · ${item.status}`));
      const details = node('dl', '', 'result-approval-details');
      for (const [label, value] of [['Tool call', item.call_id], ['Conversation', item.conversation], ['Result reference', item.reference]]) {
        details.append(node('dt', label), node('dd', value));
      }
      row.append(details);
      const actions = node('div', '', 'inline-actions');
      let consent;
      if (item.status === 'pending') {
        const label = node('label', '', 'result-approval-consent');
        consent = document.createElement('input');
        consent.type = 'checkbox';
        label.append(consent, node('span', 'I checked this result locally and want to allow it to reach the upstream model.'));
        row.append(label);
      }
      const choices = item.status === 'pending' ? [['approve', 'Allow upstream'], ['decline', 'Keep withheld']]
        : item.status === 'approved' ? [['revoke', 'Revoke allowance']] : [];
      for (const [action, title] of choices) {
        const button = node('button', title, 'btn btn-ghost');
        button.type = 'button';
        if (action === 'approve') {
          button.disabled = true;
          consent.addEventListener('change', () => { button.disabled = !consent.checked; });
        }
        button.addEventListener('click', async () => {
          if (!referenceOK(item.reference) || action === 'approve' && !consent.checked) return;
          const ticket = generation;
          const allowButton = item.status === 'pending' ? actions.children[0] : null;
          for (const sibling of actions.children) sibling.disabled = true;
          if (consent) consent.disabled = true;
          button.setAttribute('aria-busy', 'true');
          button.textContent = 'Saving…';
          try {
            // Keep this session proof out of agent discovery and plugin input.
            // An unrestricted same-user process is not isolated by CSRF alone.
            const session = await ToranaConsent.request('/_torana/api/v1/approval-session', {});
            const updated = await ToranaConsent.request(`/_torana/api/v1/approvals/${item.reference}/${action}`, {expected_status: item.status},
              (path, options) => fetch(path, {...options, headers: {...options.headers, 'X-Torana-Approval-Session': session.token}}));
            if (ticket !== generation) return;
            items = items.map(old => old.reference === updated.reference ? updated : old);
            render();
          } catch (error) {
            showAlert(error.message);
            for (const sibling of actions.children) sibling.disabled = false;
            if (consent) consent.disabled = false;
            if (allowButton) allowButton.disabled = !consent.checked;
            button.textContent = title;
            button.removeAttribute('aria-busy');
          }
        });
        actions.append(button);
      }
      if (item.status === 'withheld') row.append(node('p', 'Waiting for an agent review request. Ask your agent to request release of this reference using Torana MCP.', 'section-desc'));
      if (item.status === 'approved') row.append(node('p', 'Allowed for this exact result only. Resend the conversation from your harness to continue; a new tool call needs separate review.', 'section-desc'));
      row.append(actions);
      container.append(row);
    }
  }
  async function page(reset) {
    const ticket = ++generation;
    const more = document.getElementById('moreResultApprovals');
    more.disabled = true;
    if (reset) { cursor = ''; items = []; document.getElementById('resultApprovals').replaceChildren(node('p', 'Loading approvals…', 'empty-state')); }
    try {
      const data = await ToranaConsent.request('/_torana/api/v1/approvals?cursor=' + encodeURIComponent(cursor));
      if (ticket !== generation) return;
      items = [...items, ...(data.approvals || [])];
      cursor = data.next_cursor || '';
      more.hidden = !cursor;
      render();
    } catch (error) {
      if (ticket !== generation) return;
      showAlert(error.message);
      if (reset) document.getElementById('resultApprovals').replaceChildren(node('p', 'Approvals could not be loaded. Use Reload to try again.', 'empty-state'));
    } finally { if (ticket === generation) more.disabled = false; }
  }
  return {load: () => page(true), loadMore: () => page(false), referenceOK};
})();
