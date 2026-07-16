# Cara Cek Hasilnya di GitHub (Tanpa Perlu Paham Coding)

## Langkah 1 — Upload folder ini ke GitHub

1. Buat repository baru di GitHub (bisa private, tidak masalah)
2. Upload/push seluruh isi folder ini (termasuk folder `.github` — folder ini
   suka disembunyikan di beberapa aplikasi, pastikan ikut ter-upload)

Kalau kamu upload lewat website GitHub (drag & drop), pastikan strukturnya
tetap seperti ini setelah upload:

```
nama-repo/
├── .github/workflows/check.yml     <- WAJIB ada, ini "robot" pengecek-nya
├── backend-firebase/
├── docs/
├── rules/
└── firebase.json
```

## Langkah 2 — Tunggu robotnya jalan otomatis

Begitu upload selesai, GitHub otomatis menjalankan pengecekan. Ini butuh waktu
1-3 menit.

## Langkah 3 — Lihat hasilnya

Buka repo kamu di GitHub, lalu klik tab **"Actions"** di bagian atas halaman
(sejajar dengan tab "Code", "Issues", dll).

Di situ akan muncul daftar pengecekan dengan salah satu tanda ini di sebelah
kiri:

- 🟢 **Centang hijau** → Semua aman, kode berhasil di-compile, tidak ada
  kesalahan penulisan.
- 🔴 **Silang merah** → Ada masalah. Klik baris itu untuk buka detailnya.

## Langkah 4 — Kalau muncul silang merah (🔴)

Kamu tidak perlu paham isi errornya. Cukup:

1. Klik pada baris yang bertanda merah
2. Klik step yang gagal (biasanya ada tulisan merah di step itu)
3. **Screenshot** seluruh teks error yang muncul (warna merah/kuning)
4. Kirim screenshot itu ke saya di chat ini

Saya akan baca error-nya dan perbaiki kodenya, lalu kamu tinggal upload ulang.
Prosesnya diulang sampai semua tanda jadi hijau.

## Kenapa ada 2 pengecekan (2 baris di tab Actions)?

- **"Cek kode Go (backend-firebase)"** → mengecek kode Go (bahasa
  pemrograman backend) bisa di-compile tanpa error
- **"Cek firestore.rules (syntax)"** → mengecek aturan keamanan database
  (siapa boleh baca/tulis data apa) sudah ditulis dengan benar

Keduanya harus hijau sebelum kode ini dianggap "siap" untuk tahap berikutnya.
