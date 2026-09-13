# Upgrade notes

## Provider base URLs

Provider base URLs containing a query string, fragment, or userinfo now fail
configuration validation. Existing configurations with these components will
fail startup until corrected. A rejected hot reload leaves the running
configuration in place.

Use a base URL containing only the supported scheme, host, and path. Put
supported query parameters on individual request URLs, and configure credentials
through the provider authentication settings rather than URL userinfo.
Provider-level query defaults (for example Azure `api-version`) are not added
by this change; clients must supply required request query parameters.
