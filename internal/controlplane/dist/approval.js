globalThis.ToranaApproval = (() => {
  let active = false;
  function element(tag, text, className) {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
  }
  async function confirm(review) {
    if (active) return false;
    active = true;
    const previousFocus = document.activeElement;
    const dialog = element('dialog', '', 'approval-dialog');
    dialog.setAttribute('aria-labelledby', 'approval-title');
    dialog.setAttribute('aria-describedby', 'approval-description');
    const title = element('h2', `Enable ${review.name}?`);
    title.id = 'approval-title';
    const description = element('p', 'Review the access you are granting to this installed plugin. Approval and enablement are saved together.');
    description.id = 'approval-description';
    dialog.append(title, description);
    const access = element('ul', '', 'approval-access');
    const permissions = review.permissions;
    for (const permission of permissions) {
      const description = review.permissionDetails?.find(item => item.name === permission)?.description;
      access.appendChild(element('li', description ? `${description} (${permission})` : permission));
    }
    if (!permissions.length) access.appendChild(element('li', 'No capability grants requested.'));
    dialog.appendChild(access);
    const resources = element('ul', '', 'approval-access');
    for (const [path, grant] of Object.entries(review.resources.files || {})) {
      resources.appendChild(element('li', `${path}: ${grant.max_bytes.toLocaleString()} bytes per file, up to ${grant.retained_files} retained rotations in addition to the current file.`));
    }
    for (const [group, label] of [['credentials', 'Credentials'], ['http_endpoints', 'HTTP endpoints'], ['model_services', 'Model services'], ['pricing_resources', 'Pricing resources'], ['prompt_cache_policies', 'Prompt-cache policies']]) {
      const names = Object.keys(review.resources[group] || {});
      if (names.length) resources.appendChild(element('li', `${label}: ${names.join(', ')}. Review their exact bindings and limits below.`));
    }
    if (resources.childElementCount) dialog.appendChild(resources);
    dialog.appendChild(element('p', review.failureMode === 'block'
      ? 'If the plugin fails, Torana blocks the request.'
      : 'If the plugin fails, Torana passes the request through without that plugin’s changes.'));
    const details = element('details', '', 'approval-details');
    details.appendChild(element('summary', 'Technical details'));
    details.appendChild(element('p', 'All requested permissions are granted together. A changed bundle needs approval again.'));
    details.appendChild(element('pre', JSON.stringify({digest: review.digest, permissions, ...review.resources}, null, 2), 'mono'));
    if (review.requirements || review.conflicts) details.appendChild(element('pre', review.requirements + review.conflicts));
    dialog.appendChild(details);
    const actions = element('div', '', 'inline-actions approval-actions');
    const cancel = element('button', 'Cancel', 'btn'); cancel.type = 'button';
    const approve = element('button', 'Approve and enable', 'btn btn-primary'); approve.type = 'button';
    actions.append(cancel, approve); dialog.appendChild(actions);
    document.body.appendChild(dialog);
    try {
      return await new Promise(resolve => {
        let accepted = false;
        cancel.addEventListener('click', () => dialog.close());
        approve.addEventListener('click', () => { accepted = true; dialog.close(); });
        dialog.addEventListener('click', event => {
          if (event.target !== dialog) return;
          const bounds = dialog.getBoundingClientRect();
          if (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom) dialog.close();
        });
        dialog.addEventListener('close', () => resolve(accepted), {once: true});
        dialog.showModal();
        details.querySelector('summary').focus();
      });
    } finally {
      dialog.remove(); active = false;
      if (previousFocus?.isConnected) previousFocus.focus({preventScroll: true});
    }
  }
  return {confirm};
})();
