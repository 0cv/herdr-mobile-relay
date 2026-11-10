const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.25.1-491-a8fdfc3f884adbfe/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
