const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.24.0-489-c967a82a0e81be69/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
