const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.12-419-c97fff4260269ee4/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
