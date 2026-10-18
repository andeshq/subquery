-- Demo schema for Subquery. Run it as a superuser against a fresh database:
--
--   createdb app
--   psql -d app -f schema.sql
--
-- It creates the connection role Subquery uses, the role anonymous requests
-- assume, two related tables, a computed field, and an RPC function.

create role authenticator login password 'secret' noinherit;
create role anon nologin;

-- The authenticator impersonates anon, so it must be a member of it.
grant anon to authenticator;

create table authors (
  id   int primary key generated always as identity,
  name text not null
);

create table books (
  id        int primary key generated always as identity,
  title     text not null,
  year      int,
  author_id int references authors (id)
);

-- A scalar function taking a books row becomes a computed field: select it like
-- a column.
create function book_label(books) returns text
  language sql immutable
  as $$ select $1.title || ' (' || $1.year || ')' $$;

-- A function reachable as POST/GET /rpc/best_seller.
create function best_seller() returns setof books
  language sql stable
  as $$ select * from books order by year limit 1 $$;

insert into authors (name) values
  ('Ursula K. Le Guin'),
  ('Italo Calvino');

insert into books (title, year, author_id) values
  ('The Dispossessed', 1974, 1),
  ('Invisible Cities', 1972, 2);

-- Anonymous requests may use the demo tables and functions. Real deployments
-- usually impersonate a per-user role from a JWT claim instead.
grant usage on schema public to anon;
grant select, insert, update, delete on authors, books to anon;
grant usage, select on all sequences in schema public to anon;
grant execute on all functions in schema public to anon;
