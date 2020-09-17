package io.chubao.fs;

import com.sun.jna.Native;
import com.sun.jna.Pointer;

import io.chubao.fs.CfsDriver.Dirent;
import io.chubao.fs.CfsDriver.DirentArray;

public class CfsMount {
    public int O_RDONLY = 0;
    public int O_WRONLY = 1;
    public int O_RDWR = 2;
    public int O_ACCMODE = 3;
    public int O_CREAT = 100;
    public int O_TRUNC = 1000;
    public int O_APPEND = 2000;
    public int O_DIRECT = 40000;

    public int S_IFDIR = 0040000;
    public int S_IFREG = 0100000;
    public int S_IFLNK = 0120000;

    private CfsDriver driver;
    private String libpath;

    public CfsMount(String path) {
        libpath = path;
        driver = (CfsDriver) Native.load(libpath, CfsDriver.class);
    }

    public long NewClient() {
        return driver.cfs_new_client();
    }

    public int SetClient(long id, String key, String val) {
        return driver.cfs_set_client(id, key, val);
    }

    public int StartClient(long id) {
        return driver.cfs_start_client(id);
    }

    public void CloseClient(long id) {
        driver.cfs_close_client(id);
    }

    public int Chdir(long id, String path) {
        return driver.cfs_chdir(id, path);
    }

    public String Getcwd(long id) {
        return driver.cfs_getcwd(id);
    }

    public int GetAttr(long id, String path, CfsDriver.StatInfo stat) {
        return driver.cfs_getattr(id, path, stat);
    }

    public int Open(long id, String path, int flags, int mode) {
        return driver.cfs_open(id, path, flags, mode);
    }

    public void Close(long id, int fd) {
        driver.cfs_close(id, fd);
    }

    public long Write(long id, int fd, byte[] buf, long size, long offset) {
        return driver.cfs_write(id, fd, buf, size, offset);
    }

    public long Read(long id, int fd, byte[] buf, long size, long offset) {
        return driver.cfs_read(id, fd, buf, size, offset);
    }

    /*
     * Note that the memory allocated for Dirent[] must be countinuous. For example,
     * (new Dirent()).toArray(count).
     */
    public int Readdir(long id, int fd, Dirent[] dents, int count) {
        Pointer arr = dents[0].getPointer();
        DirentArray.ByValue slice = new DirentArray.ByValue();
        slice.data = arr;
        slice.len = (long) count;
        slice.cap = (long) count;

        long arrSize = driver.cfs_readdir(id, fd, slice, count);

        if (arrSize > 0) {
            for (int i = 0; i < (int) arrSize; i++) {
                dents[i].read();
            }
        }

        return (int) arrSize;
    }
}
