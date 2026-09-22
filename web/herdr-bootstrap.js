const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-386-7c6ee2fb279d21a0/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
