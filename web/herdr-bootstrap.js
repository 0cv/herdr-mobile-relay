const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.4-399-51ddf36c30aac7c7/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
