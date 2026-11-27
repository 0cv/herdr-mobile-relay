const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.27.0-566-8a0bf407b0e972e1/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
