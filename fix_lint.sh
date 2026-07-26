#!/bin/bash
sed -i 's/fmt.Fprint(w, "data: \[DONE\]\\n\\n")/_ = fmt.Fprint(w, "data: [DONE]\\n\\n")/g' internal/proxy/e2e_test.go
sed -i 's/go srv.Serve(ln)/go func() { _ = srv.Serve(ln) }()/g' internal/proxy/e2e_test.go internal/proxy/server_test.go
sed -i 's/defer srv.Shutdown(context.Background())/defer func() { _ = srv.Shutdown(context.Background()) }()/g' internal/proxy/e2e_test.go internal/proxy/server_test.go cmd/torana/main.go
sed -i 's/srv.Shutdown(shutdownCtx)/_ = srv.Shutdown(shutdownCtx)/g' cmd/torana/main.go
sed -i 's/defer resp.Body.Close()/defer func() { _ = resp.Body.Close() }()/g' internal/proxy/e2e_test.go internal/proxy/failover_test.go internal/proxy/server_test.go internal/proxy/offload_test.go
sed -i 's/resp.Body.Close()/_ = resp.Body.Close()/g' internal/proxy/failover.go internal/proxy/failover_test.go
sed -i 's/req.Body.Close()/_ = req.Body.Close()/g' internal/proxy/failover.go internal/proxy/server.go
sed -i 's/lastResp.Body.Close()/_ = lastResp.Body.Close()/g' internal/proxy/failover.go
sed -i 's/w.Write(/_ = w.Write(/g' internal/proxy/failover_test.go internal/proxy/offload_test.go internal/proxy/server.go internal/proxy/server_test.go
sed -i 's/json.Unmarshal(\[\]byte(tc.argsJSON), &args)/_ = json.Unmarshal([]byte(tc.argsJSON), \&args)/g' internal/proxy/jsonresponse.go
sed -i 's/t.Cleanup(func() { rt.Close() })/t.Cleanup(func() { _ = rt.Close() })/g' internal/proxy/jsonresponse_test.go
sed -i 's/json.NewDecoder(r.Body).Decode(&req)/_ = json.NewDecoder(r.Body).Decode(\&req)/g' internal/proxy/offload_test.go
sed -i 's/defer ln.Close()/defer func() { _ = ln.Close() }()/g' internal/proxy/server_test.go
sed -i 's/defer otelShutdown(context.Background())/defer func() { _ = otelShutdown(context.Background()) }()/g' cmd/torana/main.go
sed -i 's/defer pw.Close()/defer func() { _ = pw.Close() }()/g' internal/proxy/server.go
sed -i 's/defer upstreamBody.Close()/defer func() { _ = upstreamBody.Close() }()/g' internal/proxy/server.go
sed -i 's/r.Body.Close()/_ = r.Body.Close()/g' internal/proxy/server.go
sed -i 's/go plugin.WatchPlugins(watchCtx, cfg.Providers.Plugins.Dir, configFn, newRuntime, func(newPP \*plugin.PluginPipeline) {/go func() { _ = plugin.WatchPlugins(watchCtx, cfg.Providers.Plugins.Dir, configFn, newRuntime, func(newPP \*plugin.PluginPipeline) {/g' internal/proxy/server.go
# Add closing parenthesis for the go func wrapper in server.go for WatchPlugins
