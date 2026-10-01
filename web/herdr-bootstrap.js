const e = new URL(window.__HERDR_ENTRY__ || "/builds/0.21.3-385-49de75bc97e7ddc0/index.html", location);
  e.search = location.search;
  e.hash = location.hash;
  location.replace(e);
