const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.5-485-6064d1318317d710/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
