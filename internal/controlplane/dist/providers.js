/* Provider forms share the server's config API; they never handle secret values. */
globalThis.ToranaProviders = (() => {
  const protocols = {
    'openai-chat': ['OpenAI Chat Completions', 'openai'],
    'openai-responses': ['OpenAI Responses', 'openai'],
    anthropic: ['Anthropic Messages', 'anthropic'],
    gemini: ['Gemini', 'gemini'],
    'gemini-codeassist': ['Gemini Code Assist', 'gemini-codeassist'],
  };
  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text) node.textContent = text;
    return node;
  }
  function field(parent, label, key, value, choices, hint) {
    const wrapper = el('label', 'field');
    wrapper.appendChild(el('span', '', label));
    const input = el(choices ? 'select' : 'input', `input p-${key}`);
    if (choices) for (const [id, title] of choices) {
      const option = el('option', '', title);
      option.value = id;
      input.appendChild(option);
    }
    input.value = value ?? '';
    wrapper.appendChild(input);
    if (hint) wrapper.appendChild(el('span', 'hint', hint));
    parent.appendChild(wrapper);
    return input;
  }
  function create(name, original, formats) {
    const row = el('details', 'provider-editor');
    row.originalProvider = structuredClone(original);
    row.open = !name;
    const summary = el('summary', 'provider-summary', name || 'New provider');
    row.appendChild(summary);
    const content = el('div', 'provider-content');
    row.appendChild(content);
    const grid = el('div', 'form-grid');
    content.appendChild(grid);
    const nameInput = field(grid, 'Provider name', 'name', name);
    nameInput.addEventListener('input', () => { summary.textContent = nameInput.value.trim() || 'New provider'; });
    field(grid, 'Upstream URL', 'url', original.url, null, 'The model provider’s endpoint, not Torana’s local address.');
    const format = field(grid, 'Provider format', 'format', original.format || '', formats.map(f => [f, f || 'Transparent (no inference hooks)']));
    const auth = field(grid, 'Authentication', 'auth-mode', original.auth?.mode || 'caller', [
      ['caller', 'Use harness credentials'], ['credential', 'Use stored credential'], ['none', 'No authentication'],
    ]);
    const credential = field(grid, 'Torana credential ID', 'credential', original.auth?.credential, null, 'Reference a credential stored in Torana. Do not paste an API key here.');
    const authHint = el('p', 'hint');
    content.appendChild(authHint);
    const translation = el('fieldset', 'provider-translation');
    translation.appendChild(el('legend', '', 'API translation'));
    content.appendChild(translation);
    const mode = field(translation, 'Request handling', 'translation', original.bridge ? 'bridge' : 'native', [
      ['native', 'Native API — preserve the provider’s protocol'], ['bridge', 'Use an API bridge'],
    ]);
    const bridgeFields = el('div', 'form-grid');
    translation.appendChild(bridgeFields);
    const choices = Object.entries(protocols).map(([id, [label]]) => [id, label]);
    const defaultProtocol = Object.hasOwn(protocols, original.format) ? original.format : 'openai-chat';
    const client = field(bridgeFields, 'Harness API (incoming)', 'client', original.bridge?.client || defaultProtocol, choices);
    const upstream = field(bridgeFields, 'Provider API (outgoing)', 'upstream', original.bridge?.upstream || defaultProtocol, choices);
    field(bridgeFields, 'Provider model', 'model', original.bridge?.model, null, 'Optional: send every request to this model. Leave blank to keep the harness’s model name.');
    const maxTokens = field(bridgeFields, 'Default output token limit', 'max-tokens', original.bridge?.max_tokens, null, 'For Anthropic only: used when the harness supplies no limit.');
    maxTokens.type = 'number'; maxTokens.min = '1'; maxTokens.max = '2147483647'; maxTokens.step = '1';
    const project = field(bridgeFields, 'Google Cloud project', 'project', original.bridge?.project, null, 'Required when bridging a different API to Code Assist.');
    const notice = el('p', 'hint');
    translation.appendChild(notice);
    field(content, 'Fallback providers, in order', 'fallback', (original.fallback || []).join(', '), null,
      'Comma-separated provider names. Tried on connection errors, HTTP 429, or HTTP 5xx before a successful response begins. Cross-API fallback requires a bridge-enabled primary route and compatible fallback bridges. Each target needs valid authentication and a model it serves.');
    const remove = el('button', 'btn btn-sm btn-danger', 'Remove provider');
    remove.type = 'button'; remove.addEventListener('click', () => row.remove()); content.appendChild(remove);
    function sync() {
      credential.parentElement.hidden = auth.value !== 'credential';
      credential.disabled = auth.value !== 'credential';
      authHint.textContent = auth.value === 'caller' ? 'Torana forwards the harness’s authentication. No stored credential is used.'
        : auth.value === 'none' ? 'Torana sends no provider credential. Use this only for endpoints that allow unauthenticated access.'
        : 'Torana authenticates upstream using the named credential, not the harness’s key.';
      const bridged = mode.value === 'bridge';
      bridgeFields.hidden = !bridged;
      format.disabled = bridged;
      if (bridged) format.value = protocols[upstream.value][1];
      maxTokens.parentElement.hidden = upstream.value !== 'anthropic';
      project.parentElement.hidden = upstream.value !== 'gemini-codeassist';
      notice.textContent = bridged
        ? 'The harness keeps its API; Torana translates supported inference requests and responses. Provider-native features are not all portable. Bridges do not forward model-listing, token-counting, files, or batch endpoints.'
        : 'No protocol translation. Use the same API in your harness and provider.';
      if (bridged && protocols[client.value][1] !== protocols[upstream.value][1] && auth.value === 'caller') {
        notice.textContent += ' Select a stored credential (or no authentication for a local backend): harness credentials cannot be forwarded across API families.';
      }
    }
    for (const input of [auth, mode, client, upstream]) input.addEventListener('change', sync);
    sync();
    return row;
  }
  function read(row) {
    const value = key => row.querySelector(`.p-${key}`).value.trim();
    const name = value('name');
    const invalid = (key, message) => {
      row.open = true;
      const input = row.querySelector(`.p-${key}`);
      input.focus();
      throw new Error(`${name || 'New provider'}: ${message}`);
    };
    if (!name) invalid('name', 'enter a provider name, or remove this entry.');
    if (!value('url')) invalid('url', 'enter the upstream URL.');
    const provider = structuredClone(row.originalProvider);
    provider.url = value('url');
    provider.format = value('format');
    provider.auth = {mode: value('auth-mode')};
    if (provider.auth.mode === 'credential') {
      if (!value('credential')) invalid('credential', 'enter a stored credential ID.');
      provider.auth.credential = value('credential');
    }
    delete provider.fallback;
    const fallbacks = value('fallback').split(',').map(s => s.trim()).filter(Boolean);
    if (fallbacks.length) provider.fallback = fallbacks;
    // Explicit null removes a persisted bridge; omission preserves it on the API.
    provider.bridge = null;
    if (value('translation') === 'bridge') {
      const client = value('client'), upstream = value('upstream');
      provider.format = protocols[upstream][1];
      if (protocols[client][1] !== provider.format && provider.auth.mode === 'caller') {
        invalid('auth-mode', 'cross-API-family translation needs a stored credential or no authentication.');
      }
      const bridge = {...row.originalProvider.bridge, client, upstream};
      delete bridge.model; delete bridge.max_tokens; delete bridge.project;
      if (value('model')) bridge.model = value('model');
      if (upstream === 'anthropic' && value('max-tokens')) {
        const limit = Number(value('max-tokens'));
        if (!Number.isInteger(limit) || limit < 1 || limit > 2147483647) invalid('max-tokens', 'use a whole-number output limit between 1 and 2147483647.');
        bridge.max_tokens = limit;
      }
      if (upstream === 'gemini-codeassist') {
        if (value('project')) bridge.project = value('project');
        if (client !== 'gemini-codeassist' && !bridge.project) invalid('project', 'enter a Google Cloud project for Code Assist.');
      }
      provider.bridge = bridge;
    }
    return {name, provider};
  }
  function validateFallbacks(providers) {
    for (const [name, primary] of Object.entries(providers)) for (const targetName of primary.fallback || []) {
      const target = providers[targetName];
      if (!target || targetName === name) throw new Error(`${name}: choose an existing, different provider for fallback ${targetName}.`);
      if (!primary.bridge && (target.bridge || primary.format !== target.format)) {
        throw new Error(`${name}: fallback ${targetName} needs an API bridge on the primary route too. Set the harness API and provider API explicitly.`);
      }
      if (primary.bridge && target.bridge && primary.bridge.client !== target.bridge.client) {
        throw new Error(`${name}: fallback ${targetName} must accept the same harness API (${primary.bridge.client}).`);
      }
      if (primary.bridge && !target.bridge && target.format !== primary.format) {
        throw new Error(`${name}: configure an API bridge on fallback ${targetName} with the same harness API.`);
      }
    }
  }
  return {create, read, validateFallbacks};
})();
