# webdav test server

## DELETE with If-Match (probed 2026-11-27)

Apache mod_dav refuses a stale ETag (412), but right after a write its ETags are weak (`W/"..."`) and then even the current one is refused. The generic WebDAV adapter, which serves unknown servers, does not offer a conditional delete.
