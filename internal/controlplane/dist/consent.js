globalThis.ToranaConsent = (() => {
  let generation = 0;
  function node(tag, text, className) {
    const element = document.createElement(tag);
    if (text) element.textContent = text;
    if (className) element.className = className;
    return element;
  }
  async function request(path, body, fetcher = fetch) {
    const response = await fetcher(path, {method: body ? 'POST' : 'GET', cache: 'no-store',
      headers: body ? {'Content-Type': 'application/json', 'X-Torana-Local-Request': '1'} : {},
      ...(body ? {body: JSON.stringify(body)} : {})});
    const data = await response.json();
    if (!response.ok) throw new Error(data.error?.message || `Request failed (${response.status}).`);
    const result = data.execution || data;
    if (result.error) throw new Error(result.error.message || 'The change could not be completed.');
    if (result.ok === false) throw new Error('The change did not complete. Check current configuration and change history.');
    return data;
  }
  function actionPath(kind, id, action) {
    if (!['suggestions', 'changes'].includes(kind) ||
        !(kind === 'suggestions' ? ['accept', 'dismiss'] : ['undo']).includes(action) ||
        typeof id !== 'string' || !id || /[\x00-\x20\/\\]/.test(id)) throw new Error('Invalid review action.');
    return `/_torana/api/v1/agent/${kind}/${encodeURIComponent(id)}/${action}`;
  }
  async function act(button, kind, id, action, conversation) {
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    try {
      const data = await request(actionPath(kind, id, action), {conversation_id: conversation});
      const result = data.execution || data;
      showAlert(result.summary || (action === 'accept' ? 'Choice recorded. Switch models in your harness if the suggestion recommends one.' : 'Choice recorded.'), false);
      await loadSelected();
    } catch (error) {
      showAlert(error.message);
    } finally {
      button.disabled = false;
      button.removeAttribute('aria-busy');
    }
  }
  function render(container, items, conversation, kind) {
    container.replaceChildren();
    if (!items.length) {
      container.append(node('p', kind === 'suggestions' ? 'No suggestions for this conversation.' : 'No recorded changes yet.', 'empty-state'));
      return;
    }
    for (const item of items) {
      const card = node('article', '', 'card form-section');
      if (kind === 'suggestions') {
        card.append(node('p', item.plugin === 'torana' ? 'Torana · host-generated' : `${item.plugin || 'Plugin'} · plugin suggestion`, 'section-desc'));
      }
      card.append(node('h4', item.title || `Change ${item.id}`), node('p', item.outcome || item.status, 'section-desc'));
      if (item.body) {
        const body = node('p', item.body);
        body.style.whiteSpace = 'pre-wrap';
        card.append(body);
      }
      const actions = node('div', '', 'inline-actions');
      const available = kind === 'suggestions' && item.status === 'pending'
        ? (item.kind === 'torana_setup' ? [['dismiss', 'Dismiss']] : [['accept', 'Accept'], ['dismiss', 'Dismiss']])
        : kind === 'changes' && item.status === 'applied' ? [['undo', 'Undo this change']] : [];
      for (const [action, label] of available) {
        const button = node('button', label, action === 'accept' ? 'btn btn-primary' : 'btn btn-ghost');
        button.type = 'button';
        button.addEventListener('click', () => act(button, kind, item.id, action, conversation));
        actions.append(button);
      }
      card.append(actions);
      container.append(card);
    }
  }
  async function loadSelected() {
    const ticket = ++generation;
    const conversation = document.getElementById('reviewConversation').value.trim();
    const suggestions = document.getElementById('reviewSuggestions');
    const changes = document.getElementById('reviewChanges');
    suggestions.replaceChildren(node('p', conversation ? 'Loading…' : 'Choose a conversation to review.', 'empty-state'));
    changes.replaceChildren();
    if (!conversation) return;
    const query = `?conversation_id=${encodeURIComponent(conversation)}`;
    const results = await Promise.allSettled([
      request('/_torana/api/v1/agent/suggestions' + query),
      request('/_torana/api/v1/agent/changes' + query)]);
    if (ticket !== generation) return;
    for (const [index, container, kind, field] of [[0, suggestions, 'suggestions', 'suggestions'], [1, changes, 'changes', 'changes']]) {
      const result = results[index];
      if (result.status === 'fulfilled') render(container, result.value[field] || [], conversation, kind);
      else container.replaceChildren(node('p', result.reason.message, 'empty-state'));
    }
  }
  async function load() {
    try {
      const data = await request('/_torana/api/v1/conversations');
      const input = document.getElementById('reviewConversation');
      const choices = document.getElementById('reviewConversations');
      choices.replaceChildren();
      for (const conversation of data.conversations || []) {
        const option = node('option', conversation.model || 'Conversation');
        option.value = conversation.id;
        choices.append(option);
      }
      if (!input.value && data.conversations?.length) input.value = data.conversations[0].id;
    } catch (error) { showAlert(error.message); }
    await loadSelected();
  }
  return {load, loadSelected, request, actionPath, render};
})();
