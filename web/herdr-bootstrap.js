const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.26.1-521-3a75d2ccde755520/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
