const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.23.3-477-95e93e47386dac03/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
