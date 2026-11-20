const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-517-c27eb643d76ea76c/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
