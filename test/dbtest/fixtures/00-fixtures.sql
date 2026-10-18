-- Development fixtures for the Subquery compose stack.
--
-- Roles mirror PostgREST's model: a low-privilege authenticator that can
-- switch into anon and webuser. The film schema exercises every relationship
-- shape the schema cache derives: many-to-one, one-to-many, one-to-one, and
-- many-to-many through a junction, plus a view that inherits a foreign key.

create role authenticator login password 'secret' noinherit;
create role anon nologin;
create role webuser nologin;
-- viewer holds SELECT on exactly one relation, for privilege-derived Allow tests.
create role viewer nologin;

grant anon to authenticator;
grant webuser to authenticator;
grant viewer to authenticator;

create table todos (
  id   bigint generated always as identity primary key,
  task text not null,
  done boolean not null default false
);

-- Write-pipeline fixtures: an identity key plus a column with a default
-- exercises missing-value and DEFAULT semantics.
create table notes (
  id         bigint generated always as identity primary key,
  title      text not null,
  body       text,
  score      int default 7,
  created_at timestamptz not null default now()
);

create table products (
  sku   text primary key,
  name  text not null,
  price numeric(10,2) default 0
);

insert into todos (task, done) values
  ('write the schema cache', true),
  ('serve postgres-side json', false);

create table directors (
  id        int primary key generated always as identity,
  first_name text,
  last_name  text
);

create table actors (
  id        int primary key generated always as identity,
  first_name text,
  last_name  text
);

create table films (
  id          int primary key generated always as identity,
  director_id int references directors(id),
  title       text,
  year        int,
  rating      numeric(3,1),
  tags        text[],
  meta        jsonb
);

create table technical_specs (
  film_id int references films(id) primary key,
  runtime interval,
  camera  text
);

create table roles (
  film_id  int references films(id),
  actor_id int references actors(id),
  character text,
  primary key (film_id, actor_id)
);

create view films_view as
  select id, director_id, title, year
    from films;

insert into directors (first_name, last_name) values ('Quentin', 'Tarantino');
insert into actors (first_name, last_name) values ('John', 'Travolta');
insert into films (director_id, title, year, rating, tags, meta) values
  (1, 'Pulp Fiction', 1994, 8.9, '{crime,drama}', '{"genre":"crime","awards":3}');
insert into technical_specs (film_id, runtime, camera) values (1, '2:34:00', 'Arriflex');
insert into roles (film_id, actor_id, character) values (1, 1, 'Vincent Vega');

-- RPC fixtures: scalar, table-valued (relation and TABLE(...)), void,
-- single-unnamed-json, variadic array, and defaulted arguments.
create function add_them(a integer, b integer) returns integer
  as $$ select a + b $$ language sql immutable;

create function greet_user(username text default 'guest') returns text
  as $$ select 'Hello ' || username || '!' $$ language sql stable;

create function best_films() returns setof films
  as $$ select * from films $$ language sql stable;

create function film_titles() returns table(title text, year int)
  as $$ select title, year from films $$ language sql stable;

create function mult_them(json) returns int
  as $$ select ($1->>'x')::int * ($1->>'y')::int $$ language sql immutable;

create function plus_one(variadic v int[]) returns int[]
  as $$ select array_agg(n + 1) from unnest(v) as n $$ language sql immutable;

create function with_defaults(a integer, b integer default 5, c text default 'x') returns integer
  as $$ select a + b $$ language sql immutable;

-- Volatile, and callable with no arguments so OPTIONS can resolve it.
create function bump_notes(n integer default 1) returns void
  as $$ update notes set score = score where n < 0 $$ language sql volatile;

-- A computed field: a scalar function of the row type becomes a virtual column.
create function film_label(films) returns text
  as $$ select $1.title || ' (' || coalesce($1.year::text, '?') || ')' $$ language sql stable;

-- Row-level security fixtures: a policy that reads the JWT subject from
-- request.jwt.claims, and a pre-request function that can reject a request.
create table secrets (
  id      bigint generated always as identity primary key,
  owner   text not null,
  payload text not null
);

insert into secrets (owner, payload) values
  ('user_123', 'alice secret'),
  ('user_999', 'bob secret');

alter table secrets enable row level security;
alter table secrets force row level security;

create policy secrets_owner on secrets
  for select
  using (owner = current_setting('request.jwt.claims', true)::json ->> 'sub');

create schema postgrest;

create function postgrest.check_request() returns void as $$
begin
  if current_setting('request.headers', true)::json ->> 'x-block' = 'yes' then
    raise exception 'blocked by pre-request' using errcode = 'P0001';
  end if;
end;
$$ language plpgsql;

grant usage on schema public to anon, webuser, viewer;
grant select, insert, update, delete on all tables in schema public to anon, webuser;
grant usage, select on all sequences in schema public to anon, webuser;
grant execute on all functions in schema public to anon, webuser;
grant select on films to viewer;
grant select, insert, update on notes to viewer;
grant select on secrets to anon, webuser;
-- The pre-request runs as the impersonated role, so every user role needs
-- USAGE on its schema and EXECUTE on the function.
grant usage on schema postgrest to anon, webuser, viewer;
grant execute on function postgrest.check_request() to anon, webuser, viewer;
