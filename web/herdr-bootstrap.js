const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.22.3-393-c306261ecfdc18ad/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
