// Keep API errors readable without retrying a potentially applied write.
var ToranaErrors = (() => {
  function message(body, status) {
    try {
      const error = JSON.parse(body)?.error;
      if (error?.code === 'stale_revision') {
        return 'Settings changed since this page was loaded. Reload the page, review the current settings, and try again.';
      }
      if (typeof error?.message === 'string' && error.message) return error.message;
    } catch (_) { /* A non-JSON response still needs a useful fallback. */ }
    // Do not display an upstream HTML document as a wall of text.
    if (body && !body.trimStart().startsWith('<')) return body;
    return `Request failed (HTTP ${status}). Check Torana's connection and try again.`;
  }
  return {message};
})();
