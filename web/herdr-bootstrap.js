const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.4-478-3ac9f075d8f9019f/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
