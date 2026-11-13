# sandbox list testdata

`sandbox_list_page1.json` and `sandbox_list_page2.json` are two pages of
`openshell -g k8s sandbox list --page-size 2 -o json`, captured by seagull from
the live gateway on 2026-11-13 at about 04:30 UTC.

- Page one carries `next_page_token` and the sandboxes `gt-dementus` and
  `gt-furiosa`. Page two has no token and holds `gt-slit`.
- Only the `name` and `phase` fields of each row were recorded in the capture.
  Other fields the gateway returns (id, created_at, labels, conditions) are not
  in these files.
- The token text is the prefix seagull recorded, not the full token. The
  parser treats it as opaque, and the test passes it back verbatim.
- The production call adds `--page-size 100` and no workspace flag, so it lists
  the default workspace the rest of the CLI acts in. The captured pages were
  taken at page size 2 so that the listing spans two pages.
