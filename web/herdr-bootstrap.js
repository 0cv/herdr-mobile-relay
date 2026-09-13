const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.1-380-9806ec18aaa4bdcd/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
