const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-384-b7fcc9b0870f0180/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
