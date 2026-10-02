--
-- PostgreSQL database cluster dump
--

\restrict 97ulf9ZFLy3eS9DXK9ZOc80BifNbw9JtcYJJyczZvl8v8TovKfCbBouSxZMat1J

SET default_transaction_read_only = off;

SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;

--
-- Roles
--

CREATE ROLE postgres;
ALTER ROLE postgres WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS;

--
-- User Configurations
--








\unrestrict 97ulf9ZFLy3eS9DXK9ZOc80BifNbw9JtcYJJyczZvl8v8TovKfCbBouSxZMat1J

--
-- Databases
--

--
-- Database "template1" dump
--

\connect template1

--
-- PostgreSQL database dump
--

\restrict QWRQtARAxN3LcoBagwfGoxGXctfq0ldUSanigc9tceaU9U4RM41jG8vnCW7U6Ez

-- Dumped from database version 18.6 (Debian 18.6-1.pgdg13+2)
-- Dumped by pg_dump version 18.6 (Debian 18.6-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- PostgreSQL database dump complete
--

\unrestrict QWRQtARAxN3LcoBagwfGoxGXctfq0ldUSanigc9tceaU9U4RM41jG8vnCW7U6Ez

--
-- Database "loja" dump
--

--
-- PostgreSQL database dump
--

\restrict yx9eE8nsDJHgsQH6r0JdT0B1x5mjmlhQC1f2wZkic3AwLADRFMkRh6vVykPLDel

-- Dumped from database version 18.6 (Debian 18.6-1.pgdg13+2)
-- Dumped by pg_dump version 18.6 (Debian 18.6-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: loja; Type: DATABASE; Schema: -; Owner: postgres
--

CREATE DATABASE loja WITH TEMPLATE = template0 ENCODING = 'UTF8' LOCALE_PROVIDER = libc LOCALE = 'en_US.utf8';


ALTER DATABASE loja OWNER TO postgres;

\unrestrict yx9eE8nsDJHgsQH6r0JdT0B1x5mjmlhQC1f2wZkic3AwLADRFMkRh6vVykPLDel
\connect loja
\restrict yx9eE8nsDJHgsQH6r0JdT0B1x5mjmlhQC1f2wZkic3AwLADRFMkRh6vVykPLDel

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: pg_trgm; Type: EXTENSION; Schema: -; Owner: -
--

CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;


--
-- Name: EXTENSION pg_trgm; Type: COMMENT; Schema: -; Owner: 
--

COMMENT ON EXTENSION pg_trgm IS 'text similarity measurement and index searching based on trigrams';


--
-- Name: f(); Type: FUNCTION; Schema: public; Owner: postgres
--

CREATE FUNCTION public.f() RETURNS void
    LANGUAGE plpgsql
    AS $$ BEGIN
CREATE ROLE dentro_da_funcao;
END $$;


ALTER FUNCTION public.f() OWNER TO postgres;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: cliente; Type: TABLE; Schema: public; Owner: postgres
--

CREATE TABLE public.cliente (
    id integer NOT NULL,
    nome text
);


ALTER TABLE public.cliente OWNER TO postgres;

--
-- Data for Name: cliente; Type: TABLE DATA; Schema: public; Owner: postgres
--

COPY public.cliente (id, nome) FROM stdin;
1	cliente 1
2	cliente 2
3	cliente 3
4	cliente 4
5	cliente 5
6	cliente 6
7	cliente 7
8	cliente 8
9	cliente 9
10	cliente 10
11	cliente 11
12	cliente 12
13	cliente 13
14	cliente 14
15	cliente 15
16	cliente 16
17	cliente 17
18	cliente 18
19	cliente 19
20	cliente 20
\.


--
-- Name: cliente cliente_pkey; Type: CONSTRAINT; Schema: public; Owner: postgres
--

ALTER TABLE ONLY public.cliente
    ADD CONSTRAINT cliente_pkey PRIMARY KEY (id);


--
-- PostgreSQL database dump complete
--

\unrestrict yx9eE8nsDJHgsQH6r0JdT0B1x5mjmlhQC1f2wZkic3AwLADRFMkRh6vVykPLDel

--
-- Database "postgres" dump
--

\connect postgres

--
-- PostgreSQL database dump
--

\restrict GqtvOSMsJUcNjquavvlRWiV72yk84PQhrNcrBelcHEA8yMWm4L44FSZ1YkeOGVe

-- Dumped from database version 18.6 (Debian 18.6-1.pgdg13+2)
-- Dumped by pg_dump version 18.6 (Debian 18.6-1.pgdg13+2)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- PostgreSQL database dump complete
--

\unrestrict GqtvOSMsJUcNjquavvlRWiV72yk84PQhrNcrBelcHEA8yMWm4L44FSZ1YkeOGVe

--
-- PostgreSQL database cluster dump complete
--

