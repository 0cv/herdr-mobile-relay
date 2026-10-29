const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.4-480-6e9afd3ac116063a/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
