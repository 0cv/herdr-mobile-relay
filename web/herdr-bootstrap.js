const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.4-479-46f24c2012c44485/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
